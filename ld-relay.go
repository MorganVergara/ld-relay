package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"regexp"
	"strings"
	"time"
	"strconv"

	"github.com/launchdarkly/eventsource"
	"github.com/launchdarkly/gcfg"
	"github.com/streamrail/concurrent-map"
	ld "gopkg.in/launchdarkly/go-client.v2"
)

const (
	defaultRedisLocalTtlMs = 30000
	defaultPort            = 8030
)

var (
	VERSION           = "DEV"
	Debug             *log.Logger
	Info              *log.Logger
	Warning           *log.Logger
	Error             *log.Logger
	uuidHeaderPattern = regexp.MustCompile(`^(?:api_key )?((?:[a-z]{3}-)?[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89aAbB][a-f0-9]{3}-[a-f0-9]{12})$`)
	configFile        string
)

type EnvConfig struct {
	ApiKey string
	Prefix string
}

type Config struct {
	Main struct {
		ExitOnError            bool
		IgnoreConnectionErrors bool
		StreamUri              string
		BaseUri                string
		Port                   int
		HeartbeatIntervalSecs  int
	}
	Events struct {
		EventsUri         string
		SendEvents        bool
		FlushIntervalSecs int
		SamplingInterval  int32
		Capacity          int
	}
	Redis struct {
		Host     string
		Port     int
		LocalTtl *int
	}
	Environment map[string]*EnvConfig
}

type StatusEntry struct {
	Status string `json:"status"`
}

func main() {
	var port string

	flag.StringVar(&configFile, "config", "ld-relay.conf", "configuration file location")

	flag.Parse()

	initLogging(ioutil.Discard, os.Stdout, os.Stdout, os.Stderr)

	var c Config

	Info.Printf("Starting LaunchDarkly relay version %s with configuration file %s\n", formatVersion(VERSION), configFile)

	err := gcfg.ReadFileInto(&c, configFile)

	if err != nil {
		Error.Println("Failed to read configuration file. Exiting.")
		os.Exit(1)
	}

	if c.Redis.LocalTtl == nil {
		localTtl := defaultRedisLocalTtlMs
		c.Redis.LocalTtl = &localTtl
	}

	if c.Main.Port == 0 {		
		Info.Printf("No port specified in configuration file. Trying to read environment variable 'PORT'")
		port = os.Getenv("PORT")
		if (port == "") {
			Info.Printf("No 'PORT' environment variable found.  Using default port %d.", defaultPort)
			c.Main.Port= defaultPort
		} else {
			iPort, _ := strconv.Atoi(port)
			Info.Printf("Using port from environment variable, %d.", iPort)
			c.Main.Port = iPort
		}
	}

	if len(c.Environment) == 0 {
		Error.Println("You must specify at least one environment in your configuration file. Exiting.")
		os.Exit(1)
	}

	publisher := eventsource.NewServer()
	publisher.Gzip = false
	publisher.AllowCORS = true
	publisher.ReplayAll = true

	handlers := cmap.New()

	clients := cmap.New()

	eventHandlers := cmap.New()

	for envName := range c.Environment {
		clients.Set(envName, nil)
	}

	for envName, envConfig := range c.Environment {
		go func(envName string, envConfig EnvConfig) {
			var baseFeatureStore ld.FeatureStore
			if c.Redis.Host != "" && c.Redis.Port != 0 {
				Info.Printf("Using Redis Feature Store: %s:%d with prefix: %s", c.Redis.Host, c.Redis.Port, envConfig.Prefix)
				baseFeatureStore = ld.NewRedisFeatureStore(c.Redis.Host, c.Redis.Port, envConfig.Prefix, time.Duration(*c.Redis.LocalTtl)*time.Millisecond, Info)
			} else {
				baseFeatureStore = ld.NewInMemoryFeatureStore(Info)
			}

			clientConfig := ld.DefaultConfig
			clientConfig.Stream = true
			clientConfig.FeatureStore = NewSSERelayFeatureStore(envConfig.ApiKey, publisher, baseFeatureStore, c.Main.HeartbeatIntervalSecs)
			clientConfig.StreamUri = c.Main.StreamUri
			clientConfig.BaseUri = c.Main.BaseUri

			client, err := ld.MakeCustomClient(envConfig.ApiKey, clientConfig, time.Second*10)

			clients.Set(envName, client)

			if err != nil && !c.Main.IgnoreConnectionErrors {
				Error.Printf("Error initializing LaunchDarkly client for %s: %+v\n", envName, err)

				if c.Main.ExitOnError {
					os.Exit(1)
				}
			} else {
				if err != nil {
					Error.Printf("Ignoring error initializing LaunchDarkly client for %s: %+v\n", envName, err)
				}
				Info.Printf("Initialized LaunchDarkly client for %s\n", envName)
				// create a handler from the publisher for this environment
				handler := publisher.Handler(envConfig.ApiKey)
				handlers.Set(envConfig.ApiKey, handler)

				if c.Events.SendEvents {
					Info.Printf("Proxying events for environment %s", envName)
					eventHandler := newRelayHandler(envConfig.ApiKey, c)
					eventHandlers.Set(envConfig.ApiKey, eventHandler)
				}
			}
		}(envName, *envConfig)
	}

	http.Handle("/bulk", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		apiKey, err := fetchAuthToken(req)

		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if h, ok := eventHandlers.Get(apiKey); ok {
			handler := h.(http.Handler)

			handler.ServeHTTP(w, req)
		}
	}))

	http.Handle("/status", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		envs := make(map[string]StatusEntry)

		healthy := true
		for item := range clients.IterBuffered() {
			if item.Val == nil {
				envs[item.Key] = StatusEntry{Status: "disconnected"}
				healthy = false
			} else {
				client := item.Val.(*ld.LDClient)
				if client.Initialized() {
					envs[item.Key] = StatusEntry{Status: "connected"}
				} else {
					envs[item.Key] = StatusEntry{Status: "disconnected"}
					healthy = false
				}
			}
		}

		resp := make(map[string]interface{})

		resp["environments"] = envs
		if healthy {
			resp["status"] = "healthy"
		} else {
			resp["status"] = "degraded"
		}

		data, _ := json.Marshal(resp)

		w.Write(data)
	}))

	// Now make a single handler that dispatches to the appropriate handler based on the Authorization header
	http.Handle("/flags", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		apiKey, err := fetchAuthToken(req)

		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if h, ok := handlers.Get(apiKey); ok {
			handler := h.(http.Handler)

			handler.ServeHTTP(w, req)
		} else {
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}))

	Info.Printf("Listening on port %d\n", c.Main.Port)

	err = http.ListenAndServe(fmt.Sprintf(":%d", c.Main.Port), nil)
	if err != nil {
		if c.Main.ExitOnError {
			Error.Fatalf("Error starting http listener on port: %d  %s", c.Main.Port, err.Error())
		}
		Error.Printf("Error starting http listener on port: %d  %s", c.Main.Port, err.Error())
	}
}

func fetchAuthToken(req *http.Request) (string, error) {
	authHdr := req.Header.Get("Authorization")
	match := uuidHeaderPattern.FindStringSubmatch(authHdr)

	// successfully matched UUID from header
	if len(match) == 2 {
		return match[1], nil
	}

	return "", errors.New("No valid token found")
}

func formatVersion(version string) string {
	split := strings.Split(version, "+")

	if len(split) == 2 {
		return fmt.Sprintf("%s (build %s)", split[0], split[1])
	}
	return version
}

func initLogging(
	debugHandle io.Writer,
	infoHandle io.Writer,
	warningHandle io.Writer,
	errorHandle io.Writer) {

	Debug = log.New(debugHandle,
		"DEBUG: ",
		log.Ldate|log.Ltime|log.Lshortfile)

	Info = log.New(infoHandle,
		"INFO: ",
		log.Ldate|log.Ltime|log.Lshortfile)

	Warning = log.New(warningHandle,
		"WARNING: ",
		log.Ldate|log.Ltime|log.Lshortfile)

	Error = log.New(errorHandle,
		"ERROR: ",
		log.Ldate|log.Ltime|log.Lshortfile)
}

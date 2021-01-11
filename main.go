package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/golang/glog"
)

// Based on https://medium.com/ovni/writing-a-very-basic-kubernetes-mutating-admission-webhook-398dbbcb63ec

func handleRoot(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Hello, world!")
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "ok")
}

func main() {
	var params MutatingWebhookServerParams

	flag.StringVar(&params.configFile, "configFile", "/etc/webhook/config/config.yaml", "Mounted volume containing the environment configuration.")
	flag.Parse()

	config, err := loadConfig(params.configFile)
	if err != nil {
		glog.Errorf("An error occurred while loading the configuration: %v", err)
	}

	mutatingwebhookserver := &MutatingWebhookServer{
		config: config,
		server: &http.Server{
			Addr:           ":8443",
			ReadTimeout:    10 * time.Second,
			WriteTimeout:   10 * time.Second,
			MaxHeaderBytes: 1 << 20,
		},
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/_healthz", handleHealthz)
	mux.HandleFunc("/mutate", mutatingwebhookserver.mutateHandle)

	mutatingwebhookserver.server.Handler = mux

	go func() {
		if err := mutatingwebhookserver.server.ListenAndServeTLS("./certs/tls.crt", "./certs/tls.key"); err != nil {
			glog.Errorf("Unable to listen and serve the mutating webhook server: %v", err)
		} else {
			glog.Infof("Listening on :8443 for the mutating webhook server.")
		}
	}()

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	<-signalChan

	glog.Infof("Recieved the OS shutdown signal, shutting down the mutating webhook server.")
	mutatingwebhookserver.server.Shutdown(context.Background())
}

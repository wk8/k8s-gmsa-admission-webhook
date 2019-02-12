package main

// TODO wkpo clean up glide.lock ? rm -rf vendor glide.lock && make update_deps

import (
	"fmt"
	"os"

	"github.com/sirupsen/logrus"
	"k8s.io/client-go/rest"
)

func main() {
	initLogrus()

	kubeClient, err := createKubeClient()
	if err != nil {
		panic(err)
	}

	webhook := newWebhook(kubeClient)

	tlsConfig := &tlsConfig{
		crtPath: env("TLS_CRT"),
		keyPath: env("TLS_KEY"),
	}

	if err = webhook.start(443, tlsConfig); err != nil {
		panic(err)
	}
}

func initLogrus() {
	logrus.SetOutput(os.Stdout)
	// TODO wkpo higher log level for the release image, through env var
	logrus.SetLevel(logrus.DebugLevel)
}

func createKubeClient() (*kubeClient, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	return newKubeClient(config)
}

func env(key string) string {
	if value, found := os.LookupEnv(key); found {
		return value
	}
	panic(fmt.Errorf("%s env var not found", key))
}

/*
Copyright 2015 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strconv"

	"github.com/golang/glog"
	"github.com/openshift/origin/pkg/util/proc"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	clientset "k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/clientcmd"

	kcollectors "k8s.io/kube-state-metrics/pkg/collectors"
	"k8s.io/kube-state-metrics/pkg/options"
	"k8s.io/kube-state-metrics/pkg/version"
)

const (
	metricsPath = "/metrics"
	healthzPath = "/healthz"
)

// promLogger implements promhttp.Logger
type promLogger struct{}

func (pl promLogger) Println(v ...interface{}) {
	glog.Error(v...)
}

func main() {
	opts := options.NewOptions()
	opts.AddFlags()

	err := opts.Parse()
	if err != nil {
		glog.Fatalf("Error: %s", err)
	}

	if opts.Version {
		fmt.Printf("%#v\n", version.GetVersion())
		os.Exit(0)
	}

	if opts.Help {
		opts.Usage()
		os.Exit(0)
	}

	// TODO: Probably not necessary to pass all of opts into builder, right?
	collectorBuilder := kcollectors.NewBuilder(context.TODO(), opts)

	if len(opts.Collectors) == 0 {
		glog.Info("Using default collectors")
		collectorBuilder.WithEnabledCollectors(options.DefaultCollectors)
	} else {
		collectorBuilder.WithEnabledCollectors(opts.Collectors)
	}

	if len(opts.Namespaces) == 0 {
		glog.Info("Using all namespace")
		collectorBuilder.WithNamespaces(options.DefaultNamespaces)
	} else {
		if opts.Namespaces.IsAllNamespaces() {
			glog.Info("Using all namespace")
		} else {
			glog.Infof("Using %s namespaces", opts.Namespaces)
		}
		collectorBuilder.WithNamespaces(opts.Namespaces)
	}

	if opts.MetricWhitelist.IsEmpty() && opts.MetricBlacklist.IsEmpty() {
		glog.Info("No metric whitelist or blacklist set. No filtering of metrics will be done.")
	}
	if !opts.MetricWhitelist.IsEmpty() && !opts.MetricBlacklist.IsEmpty() {
		glog.Fatal("Whitelist and blacklist are both set. They are mutually exclusive, only one of them can be set.")
	}
	if !opts.MetricWhitelist.IsEmpty() {
		glog.Infof("A metric whitelist has been configured. Only the following metrics will be exposed: %s.", opts.MetricWhitelist.String())
	}
	if !opts.MetricBlacklist.IsEmpty() {
		glog.Infof("A metric blacklist has been configured. The following metrics will not be exposed: %s.", opts.MetricBlacklist.String())
	}

	proc.StartReaper()

	kubeClient, err := createKubeClient(opts.Apiserver, opts.Kubeconfig)
	if err != nil {
		glog.Fatalf("Failed to create client: %v", err)
	}
	collectorBuilder.WithKubeClient(kubeClient)

	telemetryRegistry := prometheus.NewRegistry()
	telemetryRegistry.MustRegister(
		kcollectors.ResourcesPerScrapeMetric, kcollectors.ScrapeErrorTotalMetric,
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
		prometheus.NewGoCollector(),
	)
	go telemetryServer(telemetryRegistry, opts.TelemetryHost, opts.TelemetryPort, opts.EnableGZIPEncoding)

	collectors := collectorBuilder.Build()
	ksmRegistry := prometheus.NewRegistry()
	ksmRegistry.MustRegister(collectors...)

	// TODO: Reenable white and blacklisting
	// metricsServer(metrics.FilteredGatherer(registry, opts.MetricWhitelist, opts.MetricBlacklist), opts.Host, opts.Port)
	serveMetrics(ksmRegistry, opts.Host, opts.Port, opts.EnableGZIPEncoding)
}

func createKubeClient(apiserver string, kubeconfig string) (clientset.Interface, error) {
	config, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)
	if err != nil {
		return nil, err
	}

	config.UserAgent = version.GetVersion().String()
	config.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
	config.ContentType = "application/vnd.kubernetes.protobuf"

	kubeClient, err := clientset.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	// Informers don't seem to do a good job logging error messages when it
	// can't reach the server, making debugging hard. This makes it easier to
	// figure out if apiserver is configured incorrectly.
	glog.Infof("Testing communication with server")
	v, err := kubeClient.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("ERROR communicating with apiserver: %v", err)
	}
	glog.Infof("Running with Kubernetes cluster version: v%s.%s. git version: %s. git tree state: %s. commit: %s. platform: %s",
		v.Major, v.Minor, v.GitVersion, v.GitTreeState, v.GitCommit, v.Platform)
	glog.Infof("Communication with server successful")

	return kubeClient, nil
}

func telemetryServer(registry prometheus.Gatherer, host string, port int, enableGZIPEncoding bool) {
	// Address to listen on for web interface and telemetry
	listenAddress := net.JoinHostPort(host, strconv.Itoa(port))

	glog.Infof("Starting kube-state-metrics self metrics server: %s", listenAddress)

	mux := http.NewServeMux()

	// Add metricsPath
	mux.Handle(metricsPath, promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		ErrorLog:           promLogger{},
		DisableCompression: !EnableGZIPEncoding,
	}))
	// Add index
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>Kube-State-Metrics Metrics Server</title></head>
             <body>
             <h1>Kube-State-Metrics Metrics</h1>
			 <ul>
             <li><a href='` + metricsPath + `'>metrics</a></li>
			 </ul>
             </body>
             </html>`))
	})
	log.Fatal(http.ListenAndServe(listenAddress, mux))
}

// TODO: How about accepting an interface Collector instead?
func serveMetrics(registry prometheus.Gatherer, host string, port int, EnableGZIPEncoding bool) {
	// Address to listen on for web interface and telemetry
	listenAddress := net.JoinHostPort(host, strconv.Itoa(port))

	glog.Infof("Starting metrics server: %s", listenAddress)

	mux := http.NewServeMux()

	// TODO: This doesn't belong into serveMetrics
	mux.Handle("/debug/pprof/", http.HandlerFunc(pprof.Index))
	mux.Handle("/debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
	mux.Handle("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
	mux.Handle("/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
	mux.Handle("/debug/pprof/trace", http.HandlerFunc(pprof.Trace))

	// Add metricsPath
	mux.Handle(metricsPath, promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		ErrorLog:           promLogger{},
		DisableCompression: !EnableGZIPEncoding,
	}))
	// Add healthzPath
	mux.HandleFunc(healthzPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	})
	// Add index
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>Kube Metrics Server</title></head>
             <body>
             <h1>Kube Metrics</h1>
			 <ul>
             <li><a href='` + metricsPath + `'>metrics</a></li>
             <li><a href='` + healthzPath + `'>healthz</a></li>
			 </ul>
             </body>
             </html>`))
	})
	log.Fatal(http.ListenAndServe(listenAddress, mux))
}

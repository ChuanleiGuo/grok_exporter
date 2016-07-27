package main

import (
	"flag"
	"fmt"
	"github.com/fstab/grok_exporter/exporter"
	"github.com/fstab/grok_exporter/logger"
	"github.com/fstab/grok_exporter/tailer"
	"github.com/prometheus/client_golang/prometheus"
	"net/http"
	"os"
)

var (
	showVersion = flag.Bool("version", false, "Show the grok_exporter version.")
	configPath  = flag.String("config", "", "Path to the config file. Try '-config ./example/config.yml' to get started.")
)

func main() {
	flag.Parse()
	if *showVersion {
		fmt.Printf("grok_exporter version %v build date %v.\n", exporter.VERSION, exporter.BUILD_DATE)
		return
	}
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(-1)
	}
	patterns, err := initPatterns(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(-1)
	}
	metrics, err := createMetrics(cfg, patterns)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(-1)
	}
	for _, m := range metrics {
		prometheus.MustRegister(m.Collector())
	}
	tail, err := startTailer(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(-1)
	}
	serverErrorChannel := startServer(cfg, "/metrics", prometheus.Handler())
	fmt.Printf("Starting server on %v://localhost:%v/metrics\n", cfg.Server.Protocol, cfg.Server.Port)
	err = processLogLines(tail, metrics, serverErrorChannel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err.Error())
		os.Exit(-1)
	}
}

func loadConfig() (*exporter.Config, error) {
	if *configPath == "" {
		return nil, fmt.Errorf("Usage: grok_exporter -config <path>")
	}
	return exporter.LoadConfigFile(*configPath)
}

func initPatterns(cfg *exporter.Config) (*exporter.Patterns, error) {
	patterns := exporter.InitPatterns()
	if len(cfg.Grok.PatternsDir) > 0 {
		err := patterns.AddDir(cfg.Grok.PatternsDir)
		if err != nil {
			return nil, err
		}
	}
	for _, pattern := range cfg.Grok.Patterns {
		err := patterns.AddPattern(pattern)
		if err != nil {
			return nil, err
		}
	}
	return patterns, nil
}

func createMetrics(cfg *exporter.Config, patterns *exporter.Patterns) ([]exporter.Metric, error) {
	result := make([]exporter.Metric, 0, len(*cfg.Metrics))
	for _, m := range *cfg.Metrics {
		regex, err := exporter.Compile(m.Match, patterns)
		if err != nil {
			return nil, err
		}
		switch {
		case m.Type == "counter":
			result = append(result, exporter.CreateGenericCounterVecMetric(m, regex))
		default:
			return nil, fmt.Errorf("Failed to initialize metrics: Metric type %v is not supported.\n", m.Type)
		}
	}
	return result, nil
}

func startServer(cfg *exporter.Config, path string, handler http.Handler) chan error {
	result := make(chan error)
	go func() {
		switch {
		case cfg.Server.Protocol == "http":
			result <- exporter.RunHttpServer(cfg.Server.Port, path, handler)
		case cfg.Server.Protocol == "https":
			if cfg.Server.Cert != "" && cfg.Server.Key != "" {
				result <- exporter.RunHttpsServer(cfg.Server.Port, cfg.Server.Cert, cfg.Server.Key, path, handler)
			} else {
				result <- exporter.RunHttpsServerWithDefaultKeys(cfg.Server.Port, path, handler)
			}
		default:
			// This is a bug, because cfg.validate() should make sure that protocol is either http or https.
			result <- fmt.Errorf("Configuration error: Invalid 'server.protocol': '%v'. Expecting 'http' or 'https'.", cfg.Server.Protocol)
		}
	}()
	return result
}

func startTailer(cfg *exporter.Config) (tailer.Tailer, error) {
	var tail tailer.Tailer
	switch {
	case cfg.Input.Type == "file":
		tail = tailer.RunFileTailer(cfg.Input.Path, cfg.Input.Readall, logger.New(false))
	case cfg.Input.Type == "stdin":
		tail = tailer.RunStdinTailer()
	default:
		return nil, fmt.Errorf("Config error: Input type '%v' unknown.", cfg.Input.Type)
	}
	return exporter.BufferedTailerWithMetrics(tail), nil
}

func processLogLines(tail tailer.Tailer, metrics []exporter.Metric, serverErrorChannel chan error) error {
	for {
		select {
		case err := <-serverErrorChannel:
			return fmt.Errorf("Server error: %v", err.Error())
		case err := <-tail.Errors():
			return fmt.Errorf("Error reading log lines: %v", err.Error())
		case line := <-tail.Lines():
			process(line, metrics)
		}
	}
}

func process(line string, metrics []exporter.Metric) {
	for _, metric := range metrics {
		if metric.Matches(line) {
			metric.Process(line)
		}
	}
}
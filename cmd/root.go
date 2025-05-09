package cmd

import (
	"aim/common"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	sreCommon "github.com/devopsext/sre/common"
	sreProvider "github.com/devopsext/sre/provider"
	utils "github.com/devopsext/utils"
	"github.com/spf13/cobra"
)

var (
	// Version will be set during build time
	version = "0.1.0-dev"

	APPNAME = "AIM"

	mainWG sync.WaitGroup

	// Observability components
	logs    = sreCommon.NewLogs()
	metrics = sreCommon.NewMetrics()

	stdout     *sreProvider.Stdout
	prometheus *sreProvider.PrometheusMeter
)

type RootOptions struct {
	Logs    []string
	Metrics []string
}

// Default options
var rootOptions = RootOptions{
	Logs:    strings.Split(envGet("LOGS", "stdout").(string), ","),
	Metrics: strings.Split(envGet("METRICS", "prometheus").(string), ","),
}

// Jira options with defaults
var jiraOptions = common.JiraOptions{
	URL:             envGet("JIRA_URL", "").(string),
	Username:        envGet("JIRA_USERNAME", "").(string),
	Password:        envGet("JIRA_PASSWORD", "").(string),
	ApiToken:        envGet("JIRA_API_TOKEN", "").(string),
	ProjectKey:      envGet("JIRA_PROJECT_KEY", "INCI").(string),
	QueryFilter:     envGet("JIRA_QUERY_FILTER", "").(string),
	RefreshInterval: envGet("JIRA_REFRESH_INTERVAL", 300).(int),
}

// Provider options
var stdoutOptions = sreProvider.StdoutOptions{
	Format:          envGet("STDOUT_FORMAT", "text").(string),
	Level:           envGet("STDOUT_LEVEL", "info").(string),
	Template:        envGet("STDOUT_TEMPLATE", "{{.file}} {{.msg}}").(string),
	TimestampFormat: envGet("STDOUT_TIMESTAMP_FORMAT", time.RFC3339Nano).(string),
	TextColors:      envGet("STDOUT_TEXT_COLORS", true).(bool),
}

var prometheusOptions = sreProvider.PrometheusOptions{
	URL:       envGet("PROMETHEUS_METRICS_URL", "/metrics").(string),
	Listen:    envGet("PROMETHEUS_METRICS_LISTEN", "0.0.0.0:8081").(string),
	Prefix:    envGet("PROMETHEUS_METRICS_PREFIX", "aim").(string),
	GoRuntime: envGet("PROMETHEUS_METRICS_GO_RUNTIME", true).(bool),
}

func envGet(s string, def interface{}) interface{} {
	return utils.EnvGet(fmt.Sprintf("%s_%s", APPNAME, s), def)
}

// interceptSyscall handles system signals for graceful shutdown
func interceptSyscall() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		<-c
		logs.Info("Received shutdown signal - exiting gracefully...")
		// Allow time for cleanup operations
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}()
}

func Execute() error {
	// Define the root command
	rootCmd := &cobra.Command{
		Use:   "aim",
		Short: "AIM - Analysis Issues and Metrics",
		Long: `AIM is a service that collects data from Jira,
processes it, and exposes metrics that can be scraped by Prometheus.`,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// Initialize logging
			stdoutOptions.Version = version
			stdout = sreProvider.NewStdout(stdoutOptions)
			if utils.Contains(rootOptions.Logs, "stdout") && stdout != nil {
				stdout.SetCallerOffset(2)
				logs.Register(stdout)
			}

			logs.Info("Initializing AIM service...")

			// Initialize metrics
			prometheusOptions.Version = version
			prometheus = sreProvider.NewPrometheusMeter(prometheusOptions, logs, stdout)
			if utils.Contains(rootOptions.Metrics, "prometheus") && prometheus != nil {
				prometheus.StartInWaitGroup(&mainWG)
				metrics.Register(prometheus)
				logs.Info("Prometheus metrics endpoint started at %s%s", prometheusOptions.Listen, prometheusOptions.URL)
			}

			// Validate Jira configuration
			if jiraOptions.URL == "" {
				logs.Error("Jira URL is not configured")
			}
			if jiraOptions.Username == "" || jiraOptions.ApiToken == "" {
				logs.Error("Jira credentials are not configured")
			}
		},
		Run: func(cmd *cobra.Command, args []string) {
			logs.Info("AIM service is running. Press Ctrl+C to exit.")

			// Create observability wrapper
			obs := common.NewObservability(logs, metrics)

			jiraClient, err := common.NewJiraClient(
				jiraOptions.URL,
				jiraOptions.Username,
				jiraOptions.ApiToken,
				jiraOptions.ProjectKey,
				jiraOptions.QueryFilter,
				jiraOptions.RefreshInterval,
				obs,
				metrics,
			)

			if err != nil {
				logs.Error("Failed to create Jira client: %v", err)
				os.Exit(1)
			}

			// Test the connection
			if err := jiraClient.TestConnection(); err != nil {
				logs.Error("Failed to connect to Jira: %v", err)
				// Continue anyway, might be a temporary issue
			}

			// Start the data refresh loop
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			jiraClient.StartRefreshLoop(ctx, &mainWG)
			logs.Info("Jira data collection started with refresh interval of %d seconds", jiraOptions.RefreshInterval)

			// Keep the app running
			select {}
		},
	}

	flags := rootCmd.PersistentFlags()

	// Logging flags
	flags.StringSliceVar(&rootOptions.Logs, "logs", rootOptions.Logs, "Log providers: stdout")
	flags.StringSliceVar(&rootOptions.Metrics, "metrics", rootOptions.Metrics, "Metric providers: prometheus")

	// Stdout flags
	flags.StringVar(&stdoutOptions.Format, "stdout-format", stdoutOptions.Format, "Stdout format: json, text, template")
	flags.StringVar(&stdoutOptions.Level, "stdout-level", stdoutOptions.Level, "Stdout level: info, warn, error, debug, panic")
	flags.StringVar(&stdoutOptions.Template, "stdout-template", stdoutOptions.Template, "Stdout template")
	flags.StringVar(&stdoutOptions.TimestampFormat, "stdout-timestamp-format", stdoutOptions.TimestampFormat, "Stdout timestamp format")
	flags.BoolVar(&stdoutOptions.TextColors, "stdout-text-colors", stdoutOptions.TextColors, "Stdout text colors")

	// Prometheus flags
	flags.StringVar(&prometheusOptions.URL, "prometheus-url", prometheusOptions.URL, "Prometheus endpoint URL")
	flags.StringVar(&prometheusOptions.Listen, "prometheus-listen", prometheusOptions.Listen, "Prometheus listen address and port")
	flags.StringVar(&prometheusOptions.Prefix, "prometheus-prefix", prometheusOptions.Prefix, "Prometheus metrics prefix")
	flags.BoolVar(&prometheusOptions.GoRuntime, "prometheus-go-runtime", prometheusOptions.GoRuntime, "Include Go runtime metrics")

	// Jira flags
	flags.StringVar(&jiraOptions.URL, "jira-url", jiraOptions.URL, "Jira server URL")
	flags.StringVar(&jiraOptions.Username, "jira-username", jiraOptions.Username, "Jira username")
	flags.StringVar(&jiraOptions.ApiToken, "jira-api-token", jiraOptions.ApiToken, "Jira API token")
	flags.StringVar(&jiraOptions.ProjectKey, "jira-project-key", jiraOptions.ProjectKey, "Jira project key (default: INCI)")
	flags.StringVar(&jiraOptions.QueryFilter, "jira-query-filter", jiraOptions.QueryFilter, "Additional JQL filter for Jira queries")
	flags.IntVar(&jiraOptions.RefreshInterval, "jira-refresh-interval", jiraOptions.RefreshInterval, "Interval in seconds between Jira data refreshes")

	interceptSyscall()

	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the version number",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(version)
		},
	})

	if err := rootCmd.Execute(); err != nil {
		logs.Error(err)
		os.Exit(1)
	}
}

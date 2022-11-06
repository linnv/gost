package main

import (
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/go-gost/core/logger"
	"github.com/go-gost/x/config"
	"github.com/go-gost/x/config/parsing"
	xlogger "github.com/go-gost/x/logger"
	xmetrics "github.com/go-gost/x/metrics"
	"github.com/go-gost/x/registry"
	"github.com/prometheus/common/expfmt"
	"go.uber.org/atomic"
)

var (
	log logger.Logger

	cfgFile      string
	outputFormat string
	services     stringList
	nodes        stringList
	debug        bool
	apiAddr      string
	metricsAddr  string
)

func init() {
	var printVersion bool

	flag.Var(&services, "L", "service list")
	flag.Var(&nodes, "F", "chain node list")
	flag.StringVar(&cfgFile, "C", "", "configure file")
	flag.BoolVar(&printVersion, "V", false, "print version")
	flag.StringVar(&outputFormat, "O", "", "output format, one of yaml|json format")
	flag.BoolVar(&debug, "D", false, "debug mode")
	flag.StringVar(&apiAddr, "api", "", "api service address")
	flag.StringVar(&metricsAddr, "metrics", "", "metrics service address")
	flag.Parse()

	if printVersion {
		fmt.Fprintf(os.Stdout, "gost %s (%s %s/%s)\n",
			version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		os.Exit(0)
	}

	log = xlogger.NewLogger()
	logger.SetDefault(log)
}

func main() {
	cfg := &config.Config{}
	var err error
	if len(services) > 0 || apiAddr != "" {
		cfg, err = buildConfigFromCmd(services, nodes)
		if err != nil {
			log.Fatal(err)
		}
		if debug && cfg != nil {
			if cfg.Log == nil {
				cfg.Log = &config.LogConfig{}
			}
			cfg.Log.Level = string(logger.DebugLevel)
		}
		if apiAddr != "" {
			cfg.API = &config.APIConfig{
				Addr: apiAddr,
			}
		}
		if metricsAddr != "" {
			cfg.Metrics = &config.MetricsConfig{
				Addr: metricsAddr,
			}
		}
	} else {
		if cfgFile != "" {
			err = cfg.ReadFile(cfgFile)
		} else {
			err = cfg.Load()
		}
		if err != nil {
			log.Fatal(err)
		}
	}

	log = logFromConfig(cfg.Log)

	logger.SetDefault(log)

	if outputFormat != "" {
		if err := cfg.Write(os.Stdout, outputFormat); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}

	if cfg.Profiling != nil {
		go func() {
			addr := cfg.Profiling.Addr
			if addr == "" {
				addr = ":6060"
			}
			log.Info("profiling server on ", addr)
			log.Fatal(http.ListenAndServe(addr, nil))
		}()
	}

	if cfg.API != nil {
		s, err := buildAPIService(cfg.API)
		if err != nil {
			log.Fatal(err)
		}
		defer s.Close()

		go func() {
			log.Info("api service on ", s.Addr())
			log.Fatal(s.Serve())
		}()
	}

	if cfg.Metrics != nil {
		xmetrics.Init(xmetrics.NewMetrics())
		if cfg.Metrics.Addr != "" {
			s, err := buildMetricsService(cfg.Metrics)
			if err != nil {
				log.Fatal(err)
			}
			go func() {
				defer s.Close()
				log.Info("metrics service on ", s.Addr())
				log.Fatal(s.Serve())
			}()

			go func() {
				ticker := time.NewTicker(time.Second * 6)
				var maxLimitBytes, preDayUsage float64
				var currentFlowUsageBytes atomic.Float64
				maxLimitMB := os.Getenv("MaxFlowSizeMB")
				if maxLimitMB != "" {
					if maxLimitMbUint, err := strconv.ParseInt(maxLimitMB, 10, 64); err != nil {
						const defaultFlowLimitMB = 1024
						maxLimitBytes = float64(defaultFlowLimitMB * 1024 * 1024)
						log.Infof("err: %+v and set default flow limit %d\n", err, defaultFlowLimitMB)
					} else {
						maxLimitBytes = float64(maxLimitMbUint * 1024 * 1024)
						log.Infof("set flow limit to %v MB\n", maxLimitMbUint)
					}
					go func() {
						ticker := time.NewTicker(time.Second * 58)
						for {
							select {
							case <-ticker.C:
								timenow := time.Now()
								if timenow.Hour() == 0 && timenow.Minute() == 0 {
									log.Infof("reset flow limit(max %d)", maxLimitBytes)
									preDayUsage = currentFlowUsageBytes.Load()
									currentFlowUsageBytes.Store(0)
								}
							}
						}
					}()
				}
				for {
					select {
					case <-ticker.C:
						resp, err := http.Get("http://127.0.0.1" + cfg.Metrics.Addr + "/metrics")
						if err != nil {
							log.Error(err)
							continue
						}
						var parser expfmt.TextParser
						mf, err := parser.TextToMetricFamilies(resp.Body)
						if err != nil {
							log.Error(err)
							resp.Body.Close()
							continue
						}
						resp.Body.Close()
						var total float64
						for k, v := range mf {
							if k == string(xmetrics.MetricServiceTransferOutputBytesCounter) || k == string(xmetrics.MetricServiceTransferInputBytesCounter) {
								ms := v.GetMetric()
								for _, m := range ms {
									// fmt.Printf("m.Counter.GetValue(): %+v\n", m.Counter.GetValue())
									total += m.Counter.GetValue()
								}

								continue
							}
						}
						preTotal := currentFlowUsageBytes.Load()
						if preTotal != total {
							todayUsage := (total - preDayUsage)
							if todayUsage > maxLimitBytes {
								log.Infof("over flow limit %v Bytes current usage %v Bytes exit now\n", maxLimitBytes, total)
								os.Exit(1)
							}
							log.Infof("[usage] packaget:%f today: %f/%f %f", total-preTotal, todayUsage, maxLimitBytes, (todayUsage/maxLimitBytes)*100)
							currentFlowUsageBytes.Store(total)
						}
					}
				}
			}()
		}
	}

	parsing.BuildDefaultTLSConfig(cfg.TLS)

	services := buildService(cfg)
	for _, svc := range services {
		svc := svc
		go func() {
			svc.Serve()
			// svc.Close()
		}()
	}

	config.SetGlobal(cfg)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	for sig := range sigs {
		switch sig {
		case syscall.SIGHUP:
			return
		default:
			for name, srv := range registry.ServiceRegistry().GetAll() {
				srv.Close()
				log.Debugf("service %s shutdown", name)
			}
			return
		}
	}
}

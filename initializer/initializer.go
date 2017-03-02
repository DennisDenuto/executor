package initializer

import (
	"bytes"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"time"

	"code.cloudfoundry.org/archiver/compressor"
	"code.cloudfoundry.org/archiver/extractor"
	"code.cloudfoundry.org/cacheddownloader"
	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/executor"
	"code.cloudfoundry.org/executor/containermetrics"
	"code.cloudfoundry.org/executor/depot"
	"code.cloudfoundry.org/executor/depot/containerstore"
	"code.cloudfoundry.org/executor/depot/event"
	"code.cloudfoundry.org/executor/depot/metrics"
	"code.cloudfoundry.org/executor/depot/transformer"
	"code.cloudfoundry.org/executor/depot/uploader"
	"code.cloudfoundry.org/executor/gardenhealth"
	"code.cloudfoundry.org/executor/guidgen"
	"code.cloudfoundry.org/executor/initializer/configuration"
	"code.cloudfoundry.org/garden"
	GardenClient "code.cloudfoundry.org/garden/client"
	GardenConnection "code.cloudfoundry.org/garden/client/connection"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/loggregator_v2"
	"code.cloudfoundry.org/runtimeschema/metric"
	"code.cloudfoundry.org/volman/vollocal"
	"code.cloudfoundry.org/workpool"
	"github.com/cloudfoundry/systemcerts"
	"github.com/google/shlex"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
)

const (
	PingGardenInterval             = time.Second
	StalledMetricHeartbeatInterval = 5 * time.Second
	stalledDuration                = metric.Duration("StalledGardenDuration")
	maxConcurrentUploads           = 5
	metricsReportInterval          = 1 * time.Minute
)

type executorContainers struct {
	gardenClient garden.Client
	owner        string
}

func (containers *executorContainers) Containers() ([]garden.Container, error) {
	return containers.gardenClient.Containers(garden.Properties{
		containerstore.ContainerOwnerProperty: containers.owner,
	})
}

type Duration time.Duration

func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	err := json.Unmarshal(data, &s)
	if err != nil {
		return err
	}

	dur, err := time.ParseDuration(s)
	if err != nil {
		return err
	}

	*d = Duration(dur)
	return nil
}

func (d *Duration) MarshalJSON() ([]byte, error) {
	t := time.Duration(*d)
	return []byte(fmt.Sprintf(`"%s"`, t.String())), nil
}

type ExecutorConfig struct {
	loggregator_v2.MetronConfig
	AutoDiskOverheadMB                 int      `json:"auto_disk_capacity_overhead_mb"`
	CachePath                          string   `json:"cache_path,omitempty"`
	ContainerInodeLimit                uint64   `json:"container_inode_limit,omitempty"`
	ContainerMaxCpuShares              uint64   `json:"container_max_cpu_shares,omitempty"`
	ContainerMetricsReportInterval     Duration `json:"container_metrics_report_interval,omitempty"`
	ContainerOwnerName                 string   `json:"container_owner_name,omitempty"`
	ContainerReapInterval              Duration `json:"container_reap_interval,omitempty"`
	CreateWorkPoolSize                 int      `json:"create_work_pool_size,omitempty"`
	DeleteWorkPoolSize                 int      `json:"delete_work_pool_size,omitempty"`
	DiskMB                             string   `json:"disk_mb,omitempty"`
	ExportNetworkEnvVars               bool     `json:"export_network_env_vars,omitempty"`
	GardenAddr                         string   `json:"garden_addr,omitempty"`
	GardenHealthcheckCommandRetryPause Duration `json:"garden_healthcheck_command_retry_pause,omitempty"`
	GardenHealthcheckEmissionInterval  Duration `json:"garden_healthcheck_emission_interval,omitempty"`
	GardenHealthcheckInterval          Duration `json:"garden_healthcheck_interval,omitempty"`
	GardenHealthcheckProcessArgs       []string `json:"garden_healthcheck_process_args,omitempty"`
	GardenHealthcheckProcessDir        string   `json:"garden_healthcheck_process_dir"`
	GardenHealthcheckProcessEnv        []string `json:"garden_healthcheck_process_env,omitempty"`
	GardenHealthcheckProcessPath       string   `json:"garden_healthcheck_process_path"`
	GardenHealthcheckProcessUser       string   `json:"garden_healthcheck_process_user"`
	GardenHealthcheckTimeout           Duration `json:"garden_healthcheck_timeout,omitempty"`
	GardenNetwork                      string   `json:"garden_network,omitempty"`
	HealthCheckContainerOwnerName      string   `json:"healthcheck_container_owner_name,omitempty"`
	HealthCheckWorkPoolSize            int      `json:"healthcheck_work_pool_size,omitempty"`
	HealthyMonitoringInterval          Duration `json:"healthy_monitoring_interval,omitempty"`
	InstanceIdentityCAPath             string   `json:"instance_identity_ca_path,omitempty"`
	InstanceIdentityCredDir            string   `json:"instance_identity_cred_dir,omitempty"`
	InstanceIdentityPrivateKeyPath     string   `json:"instance_identity_private_key_path,omitempty"`
	MaxCacheSizeInBytes                uint64   `json:"max_cache_size_in_bytes,omitempty"`
	MaxConcurrentDownloads             int      `json:"max_concurrent_downloads,omitempty"`
	MemoryMB                           string   `json:"memory_mb,omitempty"`
	MetricsWorkPoolSize                int      `json:"metrics_work_pool_size,omitempty"`
	PathToCACertsForDownloads          string   `json:"path_to_ca_certs_for_downloads"`
	PostSetupHook                      string   `json:"post_setup_hook"`
	PostSetupUser                      string   `json:"post_setup_user"`
	ReadWorkPoolSize                   int      `json:"read_work_pool_size,omitempty"`
	ReservedExpirationTime             Duration `json:"reserved_expiration_time,omitempty"`
	SkipCertVerify                     bool     `json:"skip_cert_verify,omitempty"`
	TempDir                            string   `json:"temp_dir,omitempty"`
	TrustedSystemCertificatesPath      string   `json:"trusted_system_certificates_path"`
	UnhealthyMonitoringInterval        Duration `json:"unhealthy_monitoring_interval,omitempty"`
	VolmanDriverPaths                  string   `json:"volman_driver_paths"`
}

const (
	defaultMaxConcurrentDownloads  = 5
	defaultCreateWorkPoolSize      = 32
	defaultDeleteWorkPoolSize      = 32
	defaultReadWorkPoolSize        = 64
	defaultMetricsWorkPoolSize     = 8
	defaultHealthCheckWorkPoolSize = 64
)

var DefaultConfiguration = ExecutorConfig{
	GardenNetwork:                      "unix",
	GardenAddr:                         "/tmp/garden.sock",
	MemoryMB:                           configuration.Automatic,
	DiskMB:                             configuration.Automatic,
	TempDir:                            "/tmp",
	ReservedExpirationTime:             Duration(time.Minute),
	ContainerReapInterval:              Duration(time.Minute),
	ContainerInodeLimit:                200000,
	ContainerMaxCpuShares:              0,
	CachePath:                          "/tmp/cache",
	MaxCacheSizeInBytes:                10 * 1024 * 1024 * 1024,
	SkipCertVerify:                     false,
	HealthyMonitoringInterval:          Duration(30 * time.Second),
	UnhealthyMonitoringInterval:        Duration(500 * time.Millisecond),
	ExportNetworkEnvVars:               false,
	ContainerOwnerName:                 "executor",
	HealthCheckContainerOwnerName:      "executor-health-check",
	CreateWorkPoolSize:                 defaultCreateWorkPoolSize,
	DeleteWorkPoolSize:                 defaultDeleteWorkPoolSize,
	ReadWorkPoolSize:                   defaultReadWorkPoolSize,
	MetricsWorkPoolSize:                defaultMetricsWorkPoolSize,
	HealthCheckWorkPoolSize:            defaultHealthCheckWorkPoolSize,
	MaxConcurrentDownloads:             defaultMaxConcurrentDownloads,
	GardenHealthcheckInterval:          Duration(10 * time.Minute),
	GardenHealthcheckEmissionInterval:  Duration(30 * time.Second),
	GardenHealthcheckTimeout:           Duration(10 * time.Minute),
	GardenHealthcheckCommandRetryPause: Duration(time.Second),
	GardenHealthcheckProcessArgs:       []string{},
	GardenHealthcheckProcessEnv:        []string{},
	ContainerMetricsReportInterval:     Duration(15 * time.Second),
}

func Initialize(logger lager.Logger, config ExecutorConfig, gardenHealthcheckRootFS string, clock clock.Clock) (executor.Client, grouper.Members, error) {
	postSetupHook, err := shlex.Split(config.PostSetupHook)
	if err != nil {
		logger.Error("failed-to-parse-post-setup-hook", err)
		return nil, grouper.Members{}, err
	}

	gardenClient := GardenClient.New(GardenConnection.New(config.GardenNetwork, config.GardenAddr))
	err = waitForGarden(logger, gardenClient, clock)
	if err != nil {
		return nil, nil, err
	}

	containersFetcher := &executorContainers{
		gardenClient: gardenClient,
		owner:        config.ContainerOwnerName,
	}

	destroyContainers(gardenClient, containersFetcher, logger)

	workDir := setupWorkDir(logger, config.TempDir)

	healthCheckWorkPool, err := workpool.NewWorkPool(config.HealthCheckWorkPoolSize)
	if err != nil {
		return nil, grouper.Members{}, err
	}

	caCertPool := systemcerts.SystemRootsPool()
	if caCertPool == nil {
		caCertPool = systemcerts.NewCertPool()
	}

	if config.PathToCACertsForDownloads != "" {
		certBytes, err := ioutil.ReadFile(config.PathToCACertsForDownloads)
		if err != nil {
			return nil, grouper.Members{}, fmt.Errorf("Unable to open CA cert bundle '%s'", config.PathToCACertsForDownloads)
		}

		certBytes = bytes.TrimSpace(certBytes)

		if len(certBytes) > 0 {
			if ok := caCertPool.AppendCertsFromPEM(certBytes); !ok {
				return nil, grouper.Members{}, errors.New("unable to load CA certificate")
			}
		}
	}

	cache := cacheddownloader.NewCache(config.CachePath, int64(config.MaxCacheSizeInBytes))
	downloader := cacheddownloader.NewDownloader(10*time.Minute, int(math.MaxInt8), config.SkipCertVerify, caCertPool)
	cachedDownloader := cacheddownloader.New(
		workDir,
		downloader,
		cache,
		cacheddownloader.TarTransform,
	)

	err = cachedDownloader.RecoverState(logger.Session("downloader"))
	if err != nil {
		return nil, grouper.Members{}, err
	}

	downloadRateLimiter := make(chan struct{}, uint(config.MaxConcurrentDownloads))

	transformer := initializeTransformer(
		logger,
		cachedDownloader,
		workDir,
		downloadRateLimiter,
		maxConcurrentUploads,
		config.SkipCertVerify,
		config.ExportNetworkEnvVars,
		time.Duration(config.HealthyMonitoringInterval),
		time.Duration(config.UnhealthyMonitoringInterval),
		healthCheckWorkPool,
		clock,
		postSetupHook,
		config.PostSetupUser,
	)

	hub := event.NewHub()

	totalCapacity, err := fetchCapacity(logger, gardenClient, config)
	if err != nil {
		return nil, grouper.Members{}, err
	}

	containerConfig := containerstore.ContainerConfig{
		OwnerName:              config.ContainerOwnerName,
		INodeLimit:             config.ContainerInodeLimit,
		MaxCPUShares:           config.ContainerMaxCpuShares,
		ReservedExpirationTime: time.Duration(config.ReservedExpirationTime),
		ReapInterval:           time.Duration(config.ContainerReapInterval),
	}

	driverConfig := vollocal.NewDriverConfig()
	driverConfig.DriverPaths = filepath.SplitList(config.VolmanDriverPaths)
	volmanClient, volmanDriverSyncer := vollocal.NewServer(logger, driverConfig)

	credManager, err := CredManagerFromConfig(logger, config, clock)
	if err != nil {
		return nil, grouper.Members{}, err
	}

	metronClient, err := loggregator_v2.NewClient(logger, config.MetronConfig)
	if err != nil {
		return nil, grouper.Members{}, err
	}

	containerStore := containerstore.New(
		containerConfig,
		&totalCapacity,
		gardenClient,
		containerstore.NewDependencyManager(cachedDownloader, downloadRateLimiter),
		volmanClient,
		credManager,
		clock,
		hub,
		transformer,
		config.TrustedSystemCertificatesPath,
		metronClient,
	)

	workPoolSettings := executor.WorkPoolSettings{
		CreateWorkPoolSize:  config.CreateWorkPoolSize,
		DeleteWorkPoolSize:  config.DeleteWorkPoolSize,
		ReadWorkPoolSize:    config.ReadWorkPoolSize,
		MetricsWorkPoolSize: config.MetricsWorkPoolSize,
	}

	depotClient := depot.NewClient(
		totalCapacity,
		containerStore,
		gardenClient,
		volmanClient,
		hub,
		workPoolSettings,
	)

	healthcheckSpec := garden.ProcessSpec{
		Path: config.GardenHealthcheckProcessPath,
		Args: config.GardenHealthcheckProcessArgs,
		User: config.GardenHealthcheckProcessUser,
		Env:  config.GardenHealthcheckProcessEnv,
		Dir:  config.GardenHealthcheckProcessDir,
	}

	gardenHealthcheck := gardenhealth.NewChecker(
		gardenHealthcheckRootFS,
		config.HealthCheckContainerOwnerName,
		time.Duration(config.GardenHealthcheckCommandRetryPause),
		healthcheckSpec,
		gardenClient,
		guidgen.DefaultGenerator,
	)

	return depotClient,
		grouper.Members{
			{"volman-driver-syncer", volmanDriverSyncer},
			{"metrics-reporter", &metrics.Reporter{
				ExecutorSource: depotClient,
				Interval:       metricsReportInterval,
				Clock:          clock,
				Logger:         logger,
			}},
			{"hub-closer", closeHub(hub)},
			{"container-metrics-reporter", containermetrics.NewStatsReporter(
				logger,
				time.Duration(config.ContainerMetricsReportInterval),
				clock,
				depotClient,
				metronClient,
			)},
			{"garden_health_checker", gardenhealth.NewRunner(
				time.Duration(config.GardenHealthcheckInterval),
				time.Duration(config.GardenHealthcheckEmissionInterval),
				time.Duration(config.GardenHealthcheckTimeout),
				logger,
				gardenHealthcheck,
				depotClient,
				clock,
			)},
			{"registry-pruner", containerStore.NewRegistryPruner(logger)},
			{"container-reaper", containerStore.NewContainerReaper(logger)},
		},
		nil
}

// Until we get a successful response from garden,
// periodically emit metrics saying how long we've been trying
// while retrying the connection indefinitely.
func waitForGarden(logger lager.Logger, gardenClient GardenClient.Client, clock clock.Clock) error {
	pingStart := clock.Now()
	logger = logger.Session("wait-for-garden", lager.Data{"initialTime:": pingStart})
	pingRequest := clock.NewTimer(0)
	pingResponse := make(chan error)
	heartbeatTimer := clock.NewTimer(StalledMetricHeartbeatInterval)

	for {
		select {
		case <-pingRequest.C():
			go func() {
				logger.Info("ping-garden", lager.Data{"wait-time-ns:": clock.Since(pingStart)})
				pingResponse <- gardenClient.Ping()
			}()

		case err := <-pingResponse:
			switch err.(type) {
			case nil:
				logger.Info("ping-garden-success", lager.Data{"wait-time-ns:": clock.Since(pingStart)})
				// send 0 to indicate ping responded successfully
				sendError := stalledDuration.Send(0)
				if sendError != nil {
					logger.Error("failed-to-send-stalled-duration-metric", sendError)
				}
				return nil
			case garden.UnrecoverableError:
				logger.Error("failed-to-ping-garden-with-unrecoverable-error", err)
				return err
			default:
				logger.Error("failed-to-ping-garden", err)
				pingRequest.Reset(PingGardenInterval)
			}

		case <-heartbeatTimer.C():
			logger.Info("emitting-stalled-garden-heartbeat", lager.Data{"wait-time-ns:": clock.Since(pingStart)})
			sendError := stalledDuration.Send(clock.Since(pingStart))
			if sendError != nil {
				logger.Error("failed-to-send-stalled-duration-heartbeat-metric", sendError)
			}

			heartbeatTimer.Reset(StalledMetricHeartbeatInterval)
		}
	}
}

func fetchCapacity(logger lager.Logger, gardenClient GardenClient.Client, config ExecutorConfig) (executor.ExecutorResources, error) {
	capacity, err := configuration.ConfigureCapacity(gardenClient, config.MemoryMB, config.DiskMB, config.MaxCacheSizeInBytes, config.AutoDiskOverheadMB)
	if err != nil {
		logger.Error("failed-to-configure-capacity", err)
		return executor.ExecutorResources{}, err
	}

	logger.Info("initial-capacity", lager.Data{
		"capacity": capacity,
	})

	return capacity, nil
}

func destroyContainers(gardenClient garden.Client, containersFetcher *executorContainers, logger lager.Logger) {
	logger.Info("executor-fetching-containers-to-destroy")
	containers, err := containersFetcher.Containers()
	if err != nil {
		logger.Fatal("executor-failed-to-get-containers", err)
		return
	} else {
		logger.Info("executor-fetched-containers-to-destroy", lager.Data{"num-containers": len(containers)})
	}

	for _, container := range containers {
		logger.Info("executor-destroying-container", lager.Data{"container-handle": container.Handle()})
		err := gardenClient.Destroy(container.Handle())
		if err != nil {
			logger.Fatal("executor-failed-to-destroy-container", err, lager.Data{
				"handle": container.Handle(),
			})
		} else {
			logger.Info("executor-destroyed-stray-container", lager.Data{
				"handle": container.Handle(),
			})
		}
	}
}

func setupWorkDir(logger lager.Logger, tempDir string) string {
	workDir := filepath.Join(tempDir, "executor-work")

	err := os.RemoveAll(workDir)
	if err != nil {
		logger.Error("working-dir.cleanup-failed", err)
		os.Exit(1)
	}

	err = os.MkdirAll(workDir, 0755)
	if err != nil {
		logger.Error("working-dir.create-failed", err)
		os.Exit(1)
	}

	return workDir
}

func initializeTransformer(
	logger lager.Logger,
	cache cacheddownloader.CachedDownloader,
	workDir string,
	downloadRateLimiter chan struct{},
	maxConcurrentUploads uint,
	skipSSLVerification bool,
	exportNetworkEnvVars bool,
	healthyMonitoringInterval time.Duration,
	unhealthyMonitoringInterval time.Duration,
	healthCheckWorkPool *workpool.WorkPool,
	clock clock.Clock,
	postSetupHook []string,
	postSetupUser string,
) transformer.Transformer {
	uploader := uploader.New(10*time.Minute, skipSSLVerification, logger)
	extractor := extractor.NewDetectable()
	compressor := compressor.NewTgz()

	return transformer.NewTransformer(
		cache,
		uploader,
		extractor,
		compressor,
		downloadRateLimiter,
		make(chan struct{}, maxConcurrentUploads),
		workDir,
		exportNetworkEnvVars,
		healthyMonitoringInterval,
		unhealthyMonitoringInterval,
		healthCheckWorkPool,
		clock,
		postSetupHook,
		postSetupUser,
	)
}

func closeHub(hub event.Hub) ifrit.Runner {
	return ifrit.RunFunc(func(signals <-chan os.Signal, ready chan<- struct{}) error {
		close(ready)
		<-signals
		hub.Close()
		return nil
	})
}

func CredManagerFromConfig(logger lager.Logger, config ExecutorConfig, clock clock.Clock) (containerstore.CredManager, error) {
	if config.InstanceIdentityCredDir != "" {
		logger.Info("instance-identity-enabled")
		keyData, err := ioutil.ReadFile(config.InstanceIdentityPrivateKeyPath)
		if err != nil {
			return nil, err
		}
		keyBlock, _ := pem.Decode(keyData)
		if keyBlock == nil {
			return nil, errors.New("instance ID key is not PEM-encoded")
		}
		privateKey, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
		if err != nil {
			return nil, err
		}

		certData, err := ioutil.ReadFile(config.InstanceIdentityCAPath)
		if err != nil {
			return nil, err
		}
		certBlock, _ := pem.Decode(certData)
		if certBlock == nil {
			return nil, errors.New("instance ID CA is not PEM-encoded")
		}
		certs, err := x509.ParseCertificates(certBlock.Bytes)
		if err != nil {
			return nil, err
		}

		return containerstore.NewCredManager(
			config.InstanceIdentityCredDir,
			rand.Reader,
			clock,
			certs[0],
			privateKey,
			"/etc/cf-instance-credentials",
		), nil
	}

	logger.Info("instance-identity-disabled")
	return containerstore.NewNoopCredManager(), nil
}

func (config *ExecutorConfig) Validate(logger lager.Logger) bool {
	valid := true

	if config.ContainerMaxCpuShares == 0 {
		logger.Error("max-cpu-shares-invalid", nil)
		valid = false
	}

	if config.HealthyMonitoringInterval <= 0 {
		logger.Error("healthy-monitoring-interval-invalid", nil)
		valid = false
	}

	if config.UnhealthyMonitoringInterval <= 0 {
		logger.Error("unhealthy-monitoring-interval-invalid", nil)
		valid = false
	}

	if config.GardenHealthcheckInterval <= 0 {
		logger.Error("garden-healthcheck-interval-invalid", nil)
		valid = false
	}

	if config.GardenHealthcheckProcessUser == "" {
		logger.Error("garden-healthcheck-process-user-invalid", nil)
		valid = false
	}

	if config.GardenHealthcheckProcessPath == "" {
		logger.Error("garden-healthcheck-process-path-invalid", nil)
		valid = false
	}

	if config.PostSetupHook != "" && config.PostSetupUser == "" {
		logger.Error("post-setup-hook-requires-a-user", nil)
		valid = false
	}

	return valid
}

package cmd

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	"github.com/lbryio/lbrytv-player/internal/config"
	"github.com/lbryio/lbrytv-player/internal/metrics"
	"github.com/lbryio/lbrytv-player/internal/version"
	"github.com/lbryio/lbrytv-player/pkg/app"
	"github.com/lbryio/lbrytv-player/pkg/logger"
	"github.com/lbryio/lbrytv-player/pkg/paid"
	"github.com/lbryio/lbrytv-player/player"

	"github.com/lbryio/lbry.go/v2/stream"
	"github.com/lbryio/reflector.go/peer/http3"
	"github.com/lbryio/reflector.go/store"

	"github.com/c2h5oh/datasize"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var Logger = logger.GetLogger()

var (
	bindAddress    string
	enablePrefetch bool
	enableProfile  bool
	verboseOutput  bool
	lbrynetAddress string
	paidPubKey     string

	upstreamReflector  string
	cloudFrontEndpoint string
	diskCacheDir       string
	diskCacheSize      string
	hotCacheSize       string

	rootCmd = &cobra.Command{
		Use:     "lbrytv_player",
		Short:   "media server for lbrytv",
		Version: version.FullName(),
		Run:     run,
	}
)

func init() {
	rootCmd.Flags().StringVar(&bindAddress, "bind", "0.0.0.0:8080", "address to bind HTTP server to")
	rootCmd.Flags().StringVar(&lbrynetAddress, "lbrynet", "http://localhost:5279/", "lbrynet server URL")
	rootCmd.Flags().StringVar(&paidPubKey, "paid_pubkey", "https://api.lbry.tv/api/v1/paid/pubkey", "pubkey for playing paid content")

	rootCmd.Flags().BoolVar(&enablePrefetch, "prefetch", false, "enable prefetch for blobs")
	rootCmd.Flags().BoolVar(&enableProfile, "profile", false, fmt.Sprintf("enable profiling server at %v", player.ProfileRoutePath))
	rootCmd.Flags().BoolVar(&verboseOutput, "verbose", false, fmt.Sprintf("enable verbose logging"))

	rootCmd.Flags().StringVar(&upstreamReflector, "upstream-reflector", "", "host:port of a reflector server where blobs are fetched from")
	rootCmd.Flags().StringVar(&cloudFrontEndpoint, "cloudfront-endpoint", "", "CloudFront edge endpoint for standard HTTP retrieval")
	rootCmd.Flags().StringVar(&diskCacheDir, "disk-cache-dir", "", "enable disk cache, storing blobs in dir")
	rootCmd.Flags().StringVar(&diskCacheSize, "disk-cache-size", "100MB", "max size of disk cache: 16GB, 500MB, etc.")
	rootCmd.Flags().StringVar(&hotCacheSize, "hot-cache-size", "", "enable hot cache for decrypted blobs and set max size: 16GB, 500MB, etc")

	//Live Config
	rootCmd.Flags().StringVar(&config.UserName, "config-username", "lbry", "Username to access the config endpoint with")
	rootCmd.Flags().StringVar(&config.Password, "config-password", "lbry", "Password to access the config endpoint with")
	rootCmd.Flags().Float64Var(&player.ThrottleScale, "throttle-scale", 1.5, "Throttle scale to rate limit in MB/s, only the 1.2 in 1.2MB/s")
	rootCmd.Flags().BoolVar(&player.ThrottleSwitch, "throttle-enabled", true, "Enables throttling")
}

func run(cmd *cobra.Command, args []string) {
	initLogger()
	defer logger.Flush()

	initPubkey()

	blobSource := getBlobSource()

	p := player.NewPlayer(initHotCache(blobSource), lbrynetAddress)
	p.SetPrefetch(enablePrefetch)

	a := app.New(app.Opts{Address: bindAddress, BlobStore: blobSource})

	player.InstallPlayerRoutes(a.Router, p)
	metrics.InstallRoute(a.Router)
	config.InstallConfigRoute(a.Router)
	if enableProfile {
		player.InstallProfilingRoutes(a.Router)
	}

	a.Start()
	a.ServeUntilShutdown()
}

func initHotCache(origin store.BlobStore) *player.HotCache {
	var hotCacheBytes datasize.ByteSize
	err := hotCacheBytes.UnmarshalText([]byte(hotCacheSize))
	if err != nil {
		Logger.Fatal(err)
	}
	if hotCacheBytes <= 0 {
		Logger.Fatal("hot cache size must be greater than 0. if you want to disable hot cache, you'll have to do a bit of coding")
	}

	metrics.PlayerCacheInfo(hotCacheBytes.Bytes())

	return player.NewHotCache(origin, int64(hotCacheBytes.Bytes()))
}

func getBlobSource() store.BlobStore {
	var blobSource store.BlobStore

	if upstreamReflector != "" {
		blobSource = http3.NewStore(http3.StoreOpts{
			Address: upstreamReflector,
			Timeout: 30 * time.Second,
		})
	} else if cloudFrontEndpoint != "" {
		blobSource = store.NewCloudFrontROStore(cloudFrontEndpoint)
	} else {
		Logger.Fatal("one of [--upstream-reflector|--cloudfront-endpoint] is required")
	}

	diskCacheMaxSize, diskCachePath := diskCacheParams()
	if diskCacheMaxSize > 0 {
		err := os.MkdirAll(diskCachePath, os.ModePerm)
		if err != nil {
			Logger.Fatal(err)
		}
		blobSource = store.NewCachingStore(
			"player",
			blobSource,
			store.NewLRUStore("player", store.NewDiskStore(diskCachePath, 2), diskCacheMaxSize/stream.MaxBlobSize),
		)
	}

	return blobSource
}

func diskCacheParams() (int, string) {
	l := Logger

	if diskCacheDir == "" {
		return 0, ""
	}

	path := diskCacheDir
	if len(path) == 0 || path[0] != '/' {
		l.Fatal("--disk-cache-dir must start with '/'")
	}

	var maxSize datasize.ByteSize
	err := maxSize.UnmarshalText([]byte(diskCacheSize))
	if err != nil {
		l.Fatal(err)
	}
	if maxSize <= 0 {
		l.Fatal("--disk-cache-size must be more than 0")
	}

	return int(maxSize), path
}

func initLogger() {
	logLevel := logrus.InfoLevel
	if verboseOutput {
		logLevel = logrus.DebugLevel
	}
	logger.ConfigureDefaults(logLevel)
	Logger.Infof("initializing %v\n", version.FullName())
	logger.ConfigureSentry(version.Version(), logger.EnvProd)
}

func initPubkey() {
	l := Logger

	r, err := http.Get(paidPubKey)
	if err != nil {
		l.Fatal(err)
	}
	rawKey, err := ioutil.ReadAll(r.Body)
	if err != nil {
		l.Fatal(err)
	}
	err = paid.InitPubKey(rawKey)
	if err != nil {
		l.Fatal(err)
	}
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		Logger.Fatalf("error: %v\n", err)
	}
}

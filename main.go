package main

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/juju/errors"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	kingpin "gopkg.in/alecthomas/kingpin.v2"

	"mtgs/config"
	"mtgs/ntp"
	"mtgs/proxy"
	"mtgs/users"
)

var version = "dev" // this has to be set by build ld flags

var (
	app = kingpin.New("mtgs", "Multiuser MTPROTO proxy")

	debug = app.Flag("debug",
		"Run in debug mode.").
		Short('d').
		Envar("MTGS_DEBUG").
		Bool()
	verbose = app.Flag("verbose",
		"Run in verbose mode.").
		Short('v').
		Envar("MTGS_VERBOSE").
		Bool()

	consulHost = app.Flag("consul-host",
		"Which host use for consul.").
		Envar("MTGS_CONSUL_HOST").
		Default("127.0.0.1").
		String()
	consulPort = app.Flag("consul-port",
		"Which port use for consul.").
		Envar("MTGS_CONSUL_PORT").
		Default("8500").
		Uint16()

	bindIP = app.Flag("bind-ip",
		"Which IP to bind to.").
		Short('b').
		Envar("MTGS_IP").
		Default("0.0.0.0").
		IP()
	bindPort = app.Flag("port",
		"Which port to bind to.").
		Short('P').
		Envar("MTGS_PORT").
		Default("3128").
		Uint16()

	bindAPIPort = app.Flag("api-port",
		"Which port to bind to.").
		Short('p').
		Envar("MTGS_API_PORT").
		Default("8080").
		Uint16()
	apiBasepath = app.Flag("api-path",
		"Which path use for API.").
		Envar("MTGS_API_PATH").
		Default("/mtgs").
		String()
	apiToken = app.Flag("api-token",
		"Which token use for API authorization.").
		Envar("MTGS_API_TOKEN").
		Short('t').
		Default("").
		String()

	publicIPv4 = app.Flag("public-ipv4",
		"Which IPv4 address is public.").
		Short('4').
		Envar("MTGS_IPV4").
		IP()
	publicIPv4Port = app.Flag("public-ipv4-port",
		"Which IPv4 port is public. Default is 'bind-port' value.").
		Envar("MTGS_IPV4_PORT").
		Uint16()

	publicIPv6 = app.Flag("public-ipv6",
		"Which IPv6 address is public.").
		Short('6').
		Envar("MTGS_IPV6").
		IP()
	publicIPv6Port = app.Flag("public-ipv6-port",
		"Which IPv6 port is public. Default is 'bind-port' value.").
		Envar("MTGS_IPV6_PORT").
		Uint16()
	writeBufferSize = app.Flag("write-buffer",
		"Write buffer size in bytes. You can think about it as a buffer from client to Telegram.").
		Short('w').
		Envar("MTGS_BUFFER_WRITE").
		Default("65536").
		Uint32()
	readBufferSize = app.Flag("read-buffer",
		"Read buffer size in bytes. You can think about it as a buffer from Telegram to client.").
		Short('r').
		Envar("MTGS_BUFFER_READ").
		Default("131072").
		Uint32()
	secureOnly = app.Flag("secure-only",
		"Support clients with dd-secrets only.").
		Short('s').
		Envar("MTGS_SECURE_ONLY").
		Bool()

	antiReplayMaxSize = app.Flag("anti-replay-max-size",
		"Max size of antireplay cache in megabytes.").
		Envar("MTGS_ANTIREPLAY_MAXSIZE").
		Default("128").
		Int()
	antiReplayEvictionTime = app.Flag("anti-replay-eviction-time",
		"Eviction time period for obfuscated2 handshakes").
		Envar("MTGS_ANTIREPLAY_EVICTIONTIME").
		Default("168h").
		Duration()

	adtag = app.Flag("adtag",
		"ADTag of the proxy.").
		Short('a').
		Envar("MTGS_ADTAG").
		HexBytes()
)

func main() { // nolint: gocyclo
	rand.Seed(time.Now().UTC().UnixNano())
	app.Version(version)
	app.HelpFlag.Short('h')

	kingpin.MustParse(app.Parse(os.Args[1:]))

	err := setRLimit()
	if err != nil {
		zap.S().Infow(err.Error())
	}

	conf, err := config.NewConfig(*debug, *verbose,
		*writeBufferSize, *readBufferSize,
		*bindIP, *publicIPv4, *publicIPv6,
		*bindPort, *bindAPIPort, *publicIPv4Port, *publicIPv6Port,
		*apiBasepath, *apiToken,
		*consulHost, *consulPort,
		*secureOnly, *antiReplayMaxSize, *antiReplayEvictionTime,
		*adtag,
	)
	if err != nil {
		usage(err.Error())
	}

	atom := zap.NewAtomicLevel()
	switch {
	case conf.Debug:
		atom.SetLevel(zapcore.DebugLevel)
	case conf.Verbose:
		atom.SetLevel(zapcore.InfoLevel)
	default:
		atom.SetLevel(zapcore.ErrorLevel)
		gin.SetMode(gin.ReleaseMode)
	}
	encoderCfg := zap.NewProductionEncoderConfig()
	logger := zap.New(zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		zapcore.Lock(os.Stderr),
		atom,
	))
	zap.ReplaceGlobals(logger)
	defer logger.Sync() // nolint: errcheck

	zap.S().Debugw("Configuration", "config", conf)

	if conf.UseMiddleProxy() {
		zap.S().Infow("Use middle proxy connection to Telegram")
		if diff, err := ntp.Fetch(); err != nil {
			zap.S().Warnw("Could not fetch time data from NTP")
		} else {
			if diff >= time.Second {
				usage(fmt.Sprintf("You choose to use middle proxy but your clock drift (%s) "+
					"is bigger than 1 second. Please, sync your time", diff))
			}
			go ntp.AutoUpdate()
		}
	} else {
		zap.S().Infow("Use direct connection to Telegram")
	}

	users.StartAPI(conf)

	server, err := proxy.NewProxy(conf)
	if err != nil {
		panic(err)
	}

	if err := server.Serve(); err != nil {
		zap.S().Fatalw("Server stopped", "error", err)
	}
}

func setRLimit() (err error) {
	rLimit := syscall.Rlimit{}
	err = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		err = errors.Annotate(err, "Cannot get rlimit")
		return
	}
	rLimit.Cur = rLimit.Max

	err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		err = errors.Annotate(err, "Cannot set rlimit")
	}

	return
}

func usage(msg string) {
	io.WriteString(os.Stderr, msg+"\n") // nolint: errcheck, gosec
	os.Exit(1)
}

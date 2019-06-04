package proxy

import (
	"context"
	"io"
	"net"
	"sync"

	"github.com/gofrs/uuid"
	"github.com/juju/errors"
	"go.uber.org/zap"

	"mtgs/antireplay"
	"mtgs/client"
	"mtgs/config"
	"mtgs/mtproto"
	"mtgs/telegram"
	"mtgs/users"
	"mtgs/wrappers"
)

// Proxy is a core of this program.
type Proxy struct {
	antiReplayCache antireplay.Cache
	clientInit      client.Init
	tg              telegram.Telegram
	conf            *config.Config
	secrets         func() ([][]byte, error)
}

// Serve runs TCP proxy server.
func (p *Proxy) Serve() error {
	lsock, err := net.Listen("tcp", p.conf.BindAddr())
	if err != nil {
		return errors.Annotate(err, "Cannot create listen socket")
	}

	for {
		if conn, err := lsock.Accept(); err != nil {
			zap.S().Errorw("Cannot allocate incoming connection", "error", err)
		} else {
			go p.accept(conn)
		}
	}
}

func (p *Proxy) accept(conn net.Conn) {
	connID := uuid.Must(uuid.NewV4()).String()
	log := zap.S().With("connection_id", connID).Named("main")
	ctx, cancel := context.WithCancel(context.Background())

	defer func() {
		cancel()
		conn.Close() // nolint: errcheck, gosec

		if err := recover(); err != nil {
			// TODO
			// only in debug mode
			// log.Errorw("Crash of accept handler", "error", err)
		}
	}()

	log.Infow("Client connected", "addr", conn.RemoteAddr())

	secrets, err := p.secrets()
	if err != nil {
		log.Errorw("Failed to get secrets", "error", err)
	}

	clientConn, opts, err := p.clientInit(ctx, cancel, conn, connID, p.antiReplayCache, p.conf, secrets)
	if err != nil {
		log.Errorw("Cannot initialize client connection", "error", err)
		return
	}
	defer clientConn.(io.Closer).Close() // nolint: errcheck

	if p.conf.SecureOnly && opts.ConnectionType != mtproto.ConnectionTypeSecure {
		log.Errorw("Proxy supports only secure connections", "connection_type", opts.ConnectionType)
		return
	}

	serverConn, err := p.getTelegramConn(ctx, cancel, opts, connID)
	if err != nil {
		log.Errorw("Cannot initialize server connection", "error", err)
		return
	}
	defer serverConn.(io.Closer).Close() // nolint: errcheck

	go func() {
		<-ctx.Done()
		serverConn.(io.Closer).Close() // nolint: gosec
		clientConn.(io.Closer).Close() // nolint: gosec
	}()

	wait := &sync.WaitGroup{}
	wait.Add(2)

	if p.conf.UseMiddleProxy() {
		clientPacket := clientConn.(wrappers.PacketReadWriteCloser)
		serverPacket := serverConn.(wrappers.PacketReadWriteCloser)
		go p.middlePipe(clientPacket, serverPacket, wait, &opts.ReadHacks)
		p.middlePipe(serverPacket, clientPacket, wait, &opts.WriteHacks)
	} else {
		clientStream := clientConn.(wrappers.StreamReadWriteCloser)
		serverStream := serverConn.(wrappers.StreamReadWriteCloser)
		go p.directPipe(clientStream, serverStream, wait, p.conf.ReadBufferSize)
		p.directPipe(serverStream, clientStream, wait, p.conf.WriteBufferSize)
	}

	wait.Wait()

	log.Infow("Client disconnected", "addr", conn.RemoteAddr())
}

func (p *Proxy) getTelegramConn(ctx context.Context, cancel context.CancelFunc,
	opts *mtproto.ConnectionOpts, connID string) (wrappers.Wrap, error) {
	streamConn, err := p.tg.Dial(ctx, cancel, connID, opts)
	if err != nil {
		return nil, errors.Annotate(err, "Cannot dial to Telegram")
	}

	packetConn, err := p.tg.Init(opts, streamConn)
	if err != nil {
		return nil, errors.Annotate(err, "Cannot handshake telegram")
	}

	return packetConn, nil
}

func (p *Proxy) middlePipe(src wrappers.PacketReadCloser, dst io.Writer, wait *sync.WaitGroup, hacks *mtproto.Hacks) {
	defer wait.Done()

	for {
		hacks.SimpleAck = false
		hacks.QuickAck = false

		packet, err := src.Read()
		if err != nil {
			src.Logger().Warnw("Cannot read packet", "error", err)
			return
		}
		if _, err = dst.Write(packet); err != nil {
			src.Logger().Warnw("Cannot write packet", "error", err)
			return
		}
	}
}

func (p *Proxy) directPipe(src wrappers.StreamReadCloser, dst io.Writer, wait *sync.WaitGroup, bufferSize int) {
	defer wait.Done()

	buffer := make([]byte, bufferSize)
	if _, err := io.CopyBuffer(dst, src, buffer); err != nil {
		src.Logger().Warnw("Cannot pump sockets", "error", err)
	}
}

// NewProxy returns new proxy instance.
func NewProxy(conf *config.Config) (*Proxy, error) {
	var clientInit client.Init
	var tg telegram.Telegram

	cache, err := antireplay.NewCache(conf)
	if err != nil {
		return nil, errors.Annotate(err, "Cannot make proxy")
	}

	if conf.UseMiddleProxy() {
		clientInit = client.MiddleInit
		tg = telegram.NewMiddleTelegram(conf)
	} else {
		clientInit = client.DirectInit
		tg = telegram.NewDirectTelegram(conf)
	}

	return &Proxy{
		antiReplayCache: cache,
		conf:            conf,
		clientInit:      clientInit,
		tg:              tg,
		secrets:         initSecrets,
	}, nil
}

func initSecrets() ([][]byte, error) {
	var secrets [][]byte
	users, err := users.User{}.GetAll()
	if err != nil {
		return secrets, err
	}
	for _, u := range users {
		secrets = append(secrets, u.Secret)
	}
	return secrets, err
}

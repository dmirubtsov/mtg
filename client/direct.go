package client

import (
	"context"
	"net"
	"time"

	"github.com/juju/errors"

	"mtg/antireplay"
	"mtg/config"
	"mtg/mtproto"
	"mtg/obfuscated2"
	"mtg/wrappers"
)

const handshakeTimeout = 10 * time.Second

// DirectInit initializes client connection for proxy which connects to
// Telegram directly.
func DirectInit(ctx context.Context, cancel context.CancelFunc, socket net.Conn,
	connID string, antiReplayCache antireplay.Cache,
	conf *config.Config, secrets [][]byte) (wrappers.Wrap, *mtproto.ConnectionOpts, error) {
	tcpSocket := socket.(*net.TCPConn)
	if err := tcpSocket.SetNoDelay(false); err != nil {
		return nil, nil, errors.Annotate(err, "Cannot disable NO_DELAY to client socket")
	}
	if err := tcpSocket.SetReadBuffer(conf.ReadBufferSize); err != nil {
		return nil, nil, errors.Annotate(err, "Cannot set read buffer size of client socket")
	}
	if err := tcpSocket.SetWriteBuffer(conf.WriteBufferSize); err != nil {
		return nil, nil, errors.Annotate(err, "Cannot set write buffer size of client socket")
	}

	socket.SetReadDeadline(time.Now().Add(handshakeTimeout)) // nolint: errcheck, gosec
	frame, err := obfuscated2.ExtractFrame(socket)
	if err != nil {
		return nil, nil, errors.Annotate(err, "Cannot extract frame")
	}
	socket.SetReadDeadline(time.Time{}) // nolint: errcheck, gosec

	conn := wrappers.NewConn(ctx, cancel, socket, connID, wrappers.ConnPurposeClient, conf.PublicIPv4, conf.PublicIPv6)
	obfs2, connOpts, err := obfuscated2.ParseObfuscated2ClientFrame(secrets, frame)
	if err != nil {
		return nil, nil, errors.Annotate(err, "Cannot parse obfuscated frame")
	}

	if antiReplayCache.Has([]byte(frame)) {
		return nil, nil, errors.New("Replay attack is detected")
	}
	antiReplayCache.Add([]byte(frame))

	connOpts.ConnectionProto = mtproto.ConnectionProtocolAny
	connOpts.ClientAddr = conn.RemoteAddr()

	conn = wrappers.NewStreamCipher(conn, obfs2.Encryptor, obfs2.Decryptor)

	conn.Logger().Infow("Client connection initialized")

	return conn, connOpts, nil
}

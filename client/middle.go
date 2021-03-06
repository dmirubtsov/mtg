package client

import (
	"context"
	"net"

	"mtgs/antireplay"
	"mtgs/config"
	"mtgs/mtproto"
	"mtgs/wrappers"
)

// MiddleInit initializes client connection for proxy which has to
// support promoted channels, connect to Telegram middle proxies etc.
func MiddleInit(ctx context.Context, cancel context.CancelFunc, socket net.Conn,
	connID string, antiReplayCache antireplay.Cache,
	conf *config.Config, secrets [][]byte) (wrappers.Wrap, *mtproto.ConnectionOpts, error) {
	conn, opts, err := DirectInit(ctx, cancel, socket, connID, antiReplayCache, conf, secrets)
	if err != nil {
		return nil, nil, err
	}
	connStream := conn.(wrappers.StreamReadWriteCloser)

	var newConn wrappers.PacketReadWriteCloser
	switch opts.ConnectionType {
	case mtproto.ConnectionTypeAbridged:
		newConn = wrappers.NewMTProtoAbridged(connStream, opts)
	case mtproto.ConnectionTypeIntermediate:
		newConn = wrappers.NewMTProtoIntermediate(connStream, opts)
	case mtproto.ConnectionTypeSecure:
		newConn = wrappers.NewMTProtoIntermediateSecure(connStream, opts)
	default:
		panic("Unknown connection type")
	}

	opts.ConnectionProto = mtproto.ConnectionProtocolIPv4
	if socket.LocalAddr().(*net.TCPAddr).IP.To4() == nil {
		opts.ConnectionProto = mtproto.ConnectionProtocolIPv6
	}

	return newConn, opts, err
}

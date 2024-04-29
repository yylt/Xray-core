package http

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	goreality "github.com/xtls/reality"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/net/cnc"
	http_proto "github.com/xtls/xray-core/common/protocol/http"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/common/signal/done"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/reality"
	"github.com/xtls/xray-core/transport/internet/tls"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/network/netpoll"
	"github.com/cloudwego/hertz/pkg/route"
)

type Listener struct {
	server  *route.Engine
	handler internet.ConnHandler
	local   net.Addr
	config  *Config
}

func (l *Listener) Addr() net.Addr {
	return l.local
}

func (l *Listener) Close() error {
	return l.server.Close()
}

type flushWriter struct {
	w io.Writer
	d *done.Instance
}

func (fw flushWriter) Write(p []byte) (n int, err error) {
	if fw.d.Done() {
		return 0, io.ErrClosedPipe
	}

	defer func() {
		if recover() != nil {
			fw.d.Close()
			err = io.ErrClosedPipe
		}
	}()

	n, err = fw.w.Write(p)
	if f, ok := fw.w.(http.Flusher); ok && err == nil {
		f.Flush()
	}
	return
}

func (l *Listener) serverNb(ctx context.Context, c *app.RequestContext) {
	host := c.Request.Host()
	if !l.config.isValidHost(string(host)) {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	path := l.config.getNormalizedPath()
	if !strings.HasPrefix(string(c.Path()), path) {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.Header("Cache-Control", "no-store")
	for _, httpHeader := range l.config.Header {
		for _, httpHeaderValue := range httpHeader.Value {
			c.Header(httpHeader.Name, httpHeaderValue)
		}
	}
	c.SetStatusCode(200)
	c.Flush()

	var remoteAddr = l.Addr()
	dest := net.DestinationFromAddr(c.RemoteAddr())
	remoteAddr = &net.TCPAddr{
		IP:   dest.Address.IP(),
		Port: int(dest.Port),
	}

	xff := c.GetHeader("X-Forwarded-For")
	if xff != nil {
		list := bytes.Split(xff, []byte(","))
		for _, proxy := range list {
			addr := net.ParseAddress(string(proxy))
			if addr.Family().IsIP() {
				remoteAddr = &net.TCPAddr{
					IP:   addr.IP(),
					Port: 0,
				}
				break
			}
		}
	}
	var (
		reader io.Reader
		close  func() error = c.Request.CloseBodyStream
	)
	if c.Request.IsBodyStream() {
		reader = c.RequestBodyStream()
	} else {
		reader = bytes.NewReader(c.Request.Body())
	}
	done := done.New()
	conn := cnc.NewConnection(
		cnc.ConnectionOutput(reader),
		cnc.ConnectionInput(flushWriter{w: c.Response.BodyWriter(), d: done}),
		cnc.ConnectionOnClose(common.NewCustomClosable(func() error {
			done.Close()
			return close()
		})),
		cnc.ConnectionLocalAddr(l.Addr()),
		cnc.ConnectionRemoteAddr(remoteAddr),
	)
	l.handler(conn)
	<-done.Wait()
}

func (l *Listener) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	host := request.Host
	if !l.config.isValidHost(host) {
		writer.WriteHeader(404)
		return
	}
	path := l.config.getNormalizedPath()
	if !strings.HasPrefix(request.URL.Path, path) {
		writer.WriteHeader(404)
		return
	}

	writer.Header().Set("Cache-Control", "no-store")

	for _, httpHeader := range l.config.Header {
		for _, httpHeaderValue := range httpHeader.Value {
			writer.Header().Set(httpHeader.Name, httpHeaderValue)
		}
	}

	writer.WriteHeader(200)
	if f, ok := writer.(http.Flusher); ok {
		f.Flush()
	}

	remoteAddr := l.Addr()
	dest, err := net.ParseDestination(request.RemoteAddr)
	if err != nil {
		newError("failed to parse request remote addr: ", request.RemoteAddr).Base(err).WriteToLog()
	} else {
		remoteAddr = &net.TCPAddr{
			IP:   dest.Address.IP(),
			Port: int(dest.Port),
		}
	}

	forwardedAddress := http_proto.ParseXForwardedFor(request.Header)
	if len(forwardedAddress) > 0 && forwardedAddress[0].Family().IsIP() {
		remoteAddr = &net.TCPAddr{
			IP:   forwardedAddress[0].IP(),
			Port: 0,
		}
	}

	done := done.New()
	conn := cnc.NewConnection(
		cnc.ConnectionOutput(request.Body),
		cnc.ConnectionInput(flushWriter{w: writer, d: done}),
		cnc.ConnectionOnClose(common.ChainedClosable{done, request.Body}),
		cnc.ConnectionLocalAddr(l.Addr()),
		cnc.ConnectionRemoteAddr(remoteAddr),
	)
	l.handler(conn)
	<-done.Wait()
}

func Listen(ctx context.Context, address net.Address, port net.Port, streamSettings *internet.MemoryStreamConfig, handler internet.ConnHandler) (internet.Listener, error) {
	httpSettings := streamSettings.ProtocolSettings.(*Config)
	var (
		listener *Listener
		opt      = config.Options{
			TransporterNewer: netpoll.NewTransporter,
			ReadTimeout:      time.Second * 4,
		}
		middler = []app.HandlerFunc{}
	)
	if port == net.Port(0) { // unix
		opt.Network = "unix"
		opt.Addr = address.Domain()
		listener = &Listener{
			handler: handler,
			local: &net.UnixAddr{
				Name: address.Domain(),
				Net:  "unix",
			},
			config: httpSettings,
		}
	} else { // tcp
		opt.Network = "tcp"
		opt.Addr = serial.Concat(address, ":", port)
		listener = &Listener{
			handler: handler,
			local: &net.TCPAddr{
				IP:   address.IP(),
				Port: int(port),
			},
			config: httpSettings,
		}
	}

	config := tls.ConfigFromStreamSettings(streamSettings)
	if config == nil {
		opt.H2C = true
		if realcfg := reality.ConfigFromStreamSettings(streamSettings); realcfg != nil {
			middler = append(middler, func(c context.Context, ctx *app.RequestContext) {
				goreality.Server(c, ctx.GetConn(), realcfg.GetREALITYConfig())
			})
		}
	} else {
		opt.TLS = config.GetTLSConfig(tls.WithNextProto("h2"))
	}

	if streamSettings.SocketSettings != nil && streamSettings.SocketSettings.AcceptProxyProtocol {
		newError("accepting PROXY protocol").AtWarning().WriteToLog(session.ExportIDToError(ctx))
	}
	middler = append(middler, func(c context.Context, ctx *app.RequestContext) {
		listener.serverNb(c, ctx)
		ctx.Abort()
	})
	listener.server = route.NewEngine(&opt)
	listener.server.Use(middler...)

	go func() {
		err := listener.server.Run()
		if err != nil {
			newError("stopping serving").Base(err).WriteToLog(session.ExportIDToError(ctx))
		}
	}()

	return listener, nil
}

func init() {
	common.Must(internet.RegisterTransportListener(protocolName, Listen))
}

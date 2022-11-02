package http

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unsafe"

	"github.com/qiniu/httpping/command"
	"github.com/qiniu/httpping/network"
)

type TcpWrapper struct {
	d            *net.TCPConn
	count        int64
	lastWrite    time.Time
	firstRead    *time.Time
	tlsHandshake time.Duration
}

func (t *TcpWrapper) Read(b []byte) (n int, err error) {
	n, err = t.d.Read(b)
	t.count += int64(n)
	if t.firstRead == nil {
		tm := time.Now()
		t.firstRead = &tm
	}
	return
}

func (t *TcpWrapper) Write(b []byte) (n int, err error) {
	n, err = t.d.Write(b)
	t.lastWrite = time.Now()
	return
}

func (t *TcpWrapper) Close() error {
	return t.d.Close()
}

func (t *TcpWrapper) LocalAddr() net.Addr {
	return t.d.LocalAddr()
}

func (t *TcpWrapper) RemoteAddr() net.Addr {
	return t.d.RemoteAddr()
}

func (t *TcpWrapper) SetDeadline(tm time.Time) error {
	return t.d.SetDeadline(tm)
}

func (t *TcpWrapper) SetReadDeadline(tm time.Time) error {
	return t.d.SetReadDeadline(tm)
}

func (t *TcpWrapper) SetWriteDeadline(tm time.Time) error {
	return t.d.SetWriteDeadline(tm)
}

func (t *TcpWrapper) Dial(_ context.Context, _, _ string) (net.Conn, error) {
	return t, nil
}

func (t *TcpWrapper) DialTLS(ctx context.Context, _, _ string) (net.Conn, error) {
	cfg := tls.Config{InsecureSkipVerify: true}
	cl := tls.Client(t, &cfg)
	start := time.Now()
	err := cl.HandshakeContext(ctx)
	if err != nil {
		return nil, err
	}
	t.tlsHandshake = time.Since(start)
	t.firstRead = nil //reset for https
	return cl, nil
}

func (t *TcpWrapper) TTFB() time.Duration {
	return t.firstRead.Sub(t.lastWrite)
}

type HttpInfo struct {
	Server             network.TCPInfo
	Client             network.TCPInfo
	Domain             string
	Ip                 string
	Port               int
	Code               int
	Hops               uint32
	DnsTimeMs          uint32
	ConnectTimeMs      uint32
	TLSHandshakeTimeMs uint32
	TTFBMs             uint32
	ReTransmitPackets  uint32
	Speed              float32 // unit kb/s
	TotalSize          int64
	TotalTimeMs        int64
	Error              string
	PingError          string
	Loss               float32
}

func (h *HttpInfo) String() string {
	t, _ := json.MarshalIndent(h, "", "	")
	return string(t)
}

func minInt(x, y int) int {
	if x < y {
		return x
	} else {
		return y
	}
}

func readN(b io.ReadCloser, toRead int) (err error) {
	d := make([]byte, 64*1024)
	var n int
	for {
		need := minInt(len(d), toRead)
		n, err = b.Read(d[:need])
		if err != nil {
			return
		}
		toRead -= n
		if toRead <= 0 {
			return
		}
	}
	return
}

const (
	infoSize = int(unsafe.Sizeof(network.TCPInfo{}))
)

func dealWithServerTcpInfo(b io.ReadCloser, contentLength int64, tcpInfo *network.TCPInfo) (err error) {
	err = readN(b, int(contentLength)-infoSize)
	if err != nil {
		return
	}
	d := (*[infoSize]byte)(unsafe.Pointer(tcpInfo))[:]
	_, err = io.ReadFull(b, d)
	return
}

func readAll(b io.ReadCloser) (err error) {
	d := make([]byte, 64*1024)
	for {
		_, err = b.Read(d)
		if err != nil {
			break
		}
	}
	if err == io.EOF {
		err = nil
	}
	return
}

func hops(ttl uint) uint32 {
	if ttl <= 64 {
		return uint32(64 - ttl)
	} else if ttl <= 128 {
		return uint32(128 - ttl)
	} else if ttl <= 256 {
		return uint32(256 - ttl)
	} else {
		return uint32(512 - ttl)
	}
}

func HttpPingSimple(url string) (*HttpInfo, error) {
	return HttpPingGet(url, true, "")
}

func HttpPingGet(url string, ping bool, srcAddr string) (*HttpInfo, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return HttpPing(req, ping, srcAddr)
}

func HttpPingServerInfo(req *http.Request, ping bool, srcAddr string, serverSupport bool) (*HttpInfo, error) {
	pWait := make(chan int, 1)
	var httpInfo HttpInfo
	u := req.URL
	var err error
	if u.Scheme == "" {
		u, err = url.Parse("http://" + u.String())
		if err != nil {
			return nil, err
		}
		req.URL = u
	}

	dnsStart := time.Now()
	addr, err := net.ResolveIPAddr("ip", u.Hostname())
	if err != nil {
		return nil, err
	}
	httpInfo.DnsTimeMs = uint32(time.Since(dnsStart).Milliseconds())
	if ping {
		go func() {
			p, err := command.Ping(addr.String(), 1, 5, 1, srcAddr)
			if err == nil && len(p.Replies) != 0 {
				httpInfo.Hops = hops(p.Replies[0].TTL)
			} else {
				httpInfo.PingError = err.Error()
			}
			pWait <- 1
		}()
	}
	httpInfo.Domain = u.Hostname()
	httpInfo.Ip = addr.String()

	var localAddr *net.TCPAddr
	if srcAddr != "" {
		localAddr, err = net.ResolveTCPAddr("tcp", srcAddr+":0")
		if err != nil {
			return nil, err
		}
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "http" || u.Scheme == "" {
			port = "80"
		} else if u.Scheme == "https" {
			port = "443"
		}
	}
	httpInfo.Port, _ = strconv.Atoi(port)
	remoteAddr, err := net.ResolveTCPAddr("tcp", addr.String()+":"+port)
	if err != nil {
		return nil, err
	}
	connectStart := time.Now()
	tcpConn, err := net.DialTCP("tcp", localAddr, remoteAddr)
	if err != nil {
		httpInfo.Error = err.Error()
		return &httpInfo, nil
	}
	httpInfo.ConnectTimeMs = uint32(time.Since(connectStart).Milliseconds())
	w := TcpWrapper{d: tcpConn}

	client := &http.Client{Transport: &http.Transport{DialContext: w.Dial}}
	if u.Scheme == "https" {
		client = &http.Client{Transport: &http.Transport{DialTLSContext: w.DialTLS}}
	}
	if serverSupport {
		req.Header.Set("X-HTTPPING-REQUIRE", "TCPINFO")
	}

	resp, err := client.Do(req)
	if err != nil {
		httpInfo.Error = err.Error()
		return &httpInfo, nil
	}
	httpInfo.Code = resp.StatusCode
	var done string
	if serverSupport {
		done = resp.Header.Get("X-HTTPPING-TCPINFO")
	}
	defer resp.Body.Close()
	if done != "" && resp.ContentLength > 0 {
		err = dealWithServerTcpInfo(resp.Body, resp.ContentLength, &httpInfo.Server)
	} else if resp.ContentLength > 0 {
		err = readN(resp.Body, int(resp.ContentLength))
	} else {
		err = readAll(resp.Body)
	}
	if err == io.EOF {
		err = nil
	}
	if err != nil {
		httpInfo.Error = err.Error()
		return &httpInfo, nil
	}

	endTime := time.Now()
	tcpInfo, err := network.GetSockoptTCPInfo(tcpConn)
	if err != nil {
		httpInfo.Error = err.Error()
	} else {
		httpInfo.Client = *tcpInfo
	}

	httpInfo.TotalSize = w.count
	httpInfo.TTFBMs = uint32(w.TTFB().Milliseconds())
	httpInfo.TotalTimeMs = endTime.Sub(connectStart).Milliseconds()
	//use last write to calculate download speed to avoid small request that firstRead == endTime
	t := endTime.Sub(w.lastWrite).Milliseconds() - int64(httpInfo.Client.RttMs)
	if t <= 0 {
		t = 1
	}
	httpInfo.Speed = float32(float64(w.count) / float64(t))
	if ping {
		<-pWait
	}
	if u.Scheme == "https" {
		httpInfo.TLSHandshakeTimeMs = uint32(w.tlsHandshake.Milliseconds())
	}

	if done != "" && resp.ContentLength != 0 {
		if httpInfo.Server.TotalPackets == 0 {
			httpInfo.Server.TotalPackets = uint32(w.count / 1460)
		}
		if httpInfo.Server.ReTransmitPackets != 0 {
			httpInfo.Loss = float32(httpInfo.Server.ReTransmitPackets) / float32(httpInfo.Server.TotalPackets) * 100.0
		}
	}

	return &httpInfo, nil
}

func HttpPing(req *http.Request, ping bool, srcAddr string) (*HttpInfo, error) {
	return HttpPingServerInfo(req, ping, srcAddr, false)
}

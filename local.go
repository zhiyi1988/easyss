package easyss

import (
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nange/easypool"
	"github.com/nange/easyss/cipherstream"
	"github.com/nange/easyss/util"
	"github.com/patrickmn/go-cache"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/txthinking/socks5"
)

var dataHeaderPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 9)
		return buf
	},
}

func (ss *Easyss) Local() {
	listenAddr := ":" + strconv.Itoa(ss.LocalPort())
	log.Infof("starting local socks5 server at %v", listenAddr)
	log.Debugf("config:%v", *ss.config)

	socks5.Debug = true
	server, err := socks5.NewClassicServer(listenAddr, "127.0.0.1", "", "", 0, 0, 0, 60)
	if err != nil {
		log.Fatalf("new socks5 server err: %+v", err)
	}
	if err := server.Run(ss); err != nil {
		log.Fatalf("socks5 server run err: %+v", err)
	}

}

func (ss *Easyss) TCPHandle(s *socks5.Server, conn *net.TCPConn, r *socks5.Request) error {
	targetAddr := r.Address()
	log.Infof("target addr:%v", targetAddr)

	if r.Cmd == socks5.CmdConnect {
		a, addr, port, err := socks5.ParseAddress(ss.LocalAddr())
		if err != nil {
			log.Errorf("socks5 ParseAddress err:%+v", err)
			return err
		}
		p := socks5.NewReply(socks5.RepSuccess, a, addr, port)
		if err := p.WriteTo(conn); err != nil {
			return err
		}

		return ss.localRelay(conn, targetAddr)
	}

	if r.Cmd == socks5.CmdUDP {
		caddr, err := r.UDP(conn, s.ServerAddr)
		if err != nil {
			return err
		}
		_, p, err := net.SplitHostPort(caddr.String())
		if err != nil {
			return err
		}
		if p == "0" {
			time.Sleep(time.Duration(s.UDPSessionTime) * time.Second)
			return nil
		}
		ch := make(chan byte)
		s.TCPUDPAssociate.Set(caddr.String(), ch, cache.DefaultExpiration)
		<-ch
		return nil
	}

	return socks5.ErrUnsupportCmd
}

func (ss *Easyss) UDPHandle(s *socks5.Server, addr *net.UDPAddr, d *socks5.Datagram) error {
	return ss.handle.UDPHandle(s, addr, d)
}

var paddingPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, cipherstream.PaddingSize)
		return buf
	},
}

func (ss *Easyss) localRelay(localConn net.Conn, addr string) (err error) {
	var stream io.ReadWriteCloser
	stream, err = ss.tcpPool.Get()
	log.Infof("after pool get: current tcp pool have %v connections", ss.tcpPool.Len())
	defer log.Infof("after stream close: current tcp pool have %v connections", ss.tcpPool.Len())

	if err != nil {
		log.Errorf("get stream err:%+v", err)
		return
	}
	defer stream.Close()

	header := dataHeaderPool.Get().([]byte)
	defer dataHeaderPool.Put(header)

	header = util.EncodeHTTP2DataFrameHeader(len(addr)+1, header)
	gcm, err := cipherstream.NewAes256GCM([]byte(ss.config.Password))
	if err != nil {
		log.Errorf("cipherstream.NewAes256GCM err:%+v", err)
		return
	}

	headercipher, err := gcm.Encrypt(header)
	if err != nil {
		log.Errorf("gcm.Encrypt err:%+v", err)
		return
	}
	ciphermethod := EncodeCipherMethod(ss.config.Method)
	if ciphermethod == 0 {
		log.Errorf("unsupported cipher method:%+v", ss.config.Method)
		return
	}
	payloadcipher, err := gcm.Encrypt(append([]byte(addr), ciphermethod))
	if err != nil {
		log.Errorf("gcm.Encrypt err:%+v", err)
		return
	}

	handshake := append(headercipher, payloadcipher...)
	if header[4] == 0x8 { // has padding field
		padBytes := paddingPool.Get().([]byte)
		defer paddingPool.Put(padBytes)

		var padcipher []byte
		padcipher, err = gcm.Encrypt(padBytes)
		if err != nil {
			log.Errorf("encrypt padding buf err:%+v", err)
			return
		}
		handshake = append(handshake, padcipher...)
	}
	_, err = stream.Write(handshake)
	if err != nil {
		log.Errorf("stream.Write err:%+v", errors.WithStack(err))
		if pc, ok := stream.(*easypool.PoolConn); ok {
			log.Infof("mark pool conn stream unusable")
			pc.MarkUnusable()
		}
		return
	}

	csStream, err := cipherstream.New(stream, ss.config.Password, ss.config.Method)
	if err != nil {
		log.Errorf("new cipherstream err:%+v, password:%v, method:%v",
			err, ss.config.Password, ss.config.Method)
		return
	}

	n1, n2, needclose := relay(csStream, localConn)
	log.Infof("send %v bytes to %v, and recive %v bytes", n1, addr, n2)
	if !needclose {
		log.Infof("underline connection is health, so reuse it")
	}
	atomic.AddInt64(&ss.stat.BytesSend, n1)
	atomic.AddInt64(&ss.stat.BytesRecive, n2)

	return
}

func EncodeCipherMethod(m string) byte {
	methodMap := map[string]byte{
		"aes-256-gcm":       1,
		"chacha20-poly1305": 2,
	}
	if b, ok := methodMap[m]; ok {
		return b
	}
	return 0
}

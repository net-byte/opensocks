package proxy

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/net-byte/opensocks/config"
)

func UDPProxy(tcpConn net.Conn, config config.Config) {
	defer tcpConn.Close()
	//bind local udp server
	localTCPAddr, _ := net.ResolveTCPAddr("tcp", tcpConn.LocalAddr().String())
	localUDPAddr := fmt.Sprintf("%s:%d", localTCPAddr.IP.String(), 0)
	udpAddr, _ := net.ResolveUDPAddr("udp", localUDPAddr)
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Println(err)
		return
	}
	defer udpConn.Close()
	bindAddr, _ := net.ResolveUDPAddr("udp", udpConn.LocalAddr().String())
	log.Printf("[udp] bind addr %v %v", bindAddr.IP.To4().String(), bindAddr.Port)

	//response to client
	response := []byte{Socks5Version, SuccessReply, 0x00, 0x01}
	buffer := bytes.NewBuffer(response)
	binary.Write(buffer, binary.BigEndian, bindAddr.IP.To4())
	binary.Write(buffer, binary.BigEndian, uint16(bindAddr.Port))
	tcpConn.Write(buffer.Bytes())

	done := make(chan bool, 0)
	go keepUDPAlive(tcpConn.(*net.TCPConn), done)
	go forwardUDP(udpConn, config)
	<-done
	log.Printf("[udp] proxy has done, addr %v %v", bindAddr.IP.To4().String(), bindAddr.Port)
}

type UDPServer struct {
	clientAddr   *net.UDPAddr
	serverConn   *net.UDPConn
	dstAddrCache sync.Map
	wsConnCache  sync.Map
	config       config.Config
}

func (udpServer *UDPServer) forwardRemote() {
	buf := make([]byte, BufferSize)
	for {
		udpServer.serverConn.SetReadDeadline(time.Now().Add(60 * time.Second))
		n, udpAddr, err := udpServer.serverConn.ReadFromUDP(buf)
		if err != nil || err == io.EOF || n == 0 {
			break
		}
		if udpServer.clientAddr == nil {
			udpServer.clientAddr = udpAddr
		}
		b := buf[:n]
		/*
		   +----+------+------+----------+----------+----------+
		   |RSV | FRAG | ATYP | DST.ADDR | DST.PORT |   DATA   |
		   +----+------+------+----------+----------+----------+
		   |  2 |   1  |   1  | Variable |     2    | Variable |
		   +----+------+------+----------+----------+----------+
		*/
		if b[2] != 0x00 {
			log.Printf("[udp] frag do not support")
			continue
		}
		var dstAddr *net.UDPAddr
		var data []byte
		switch b[3] {
		case Ipv4Address:
			dstAddr = &net.UDPAddr{
				IP:   net.IPv4(b[4], b[5], b[6], b[7]),
				Port: int(b[8])<<8 | int(b[9]),
			}
			udpServer.dstAddrCache.LoadOrStore(dstAddr.String(), string(b[0:10]))
			data = b[10:]
		case FqdnAddress:
			domainLength := int(b[4])
			domain := string(b[5 : 5+domainLength])
			ipAddr, err := net.ResolveIPAddr("ip", domain)
			if err != nil {
				log.Printf("[udp] dns %s resolve error:%v", domain, err)
				continue
			}
			dstAddr = &net.UDPAddr{
				IP:   ipAddr.IP,
				Port: int(b[5+domainLength])<<8 | int(b[6+domainLength]),
			}
			udpServer.dstAddrCache.LoadOrStore(dstAddr.String(), string(b[0:7+domainLength]))
			data = b[7+domainLength:]
		case Ipv6Address:
			{
				dstAddr = &net.UDPAddr{
					IP:   net.IP(b[4:19]),
					Port: int(b[20])<<8 | int(b[21]),
				}
				udpServer.dstAddrCache.LoadOrStore(dstAddr.String(), string(b[0:22]))
				data = b[22:]
			}
		default:
			continue
		}
		var wsConn *websocket.Conn
		if value, ok := udpServer.wsConnCache.Load(dstAddr.String()); ok {
			wsConn = value.(*websocket.Conn)
		} else {
			wsConn = ConnectWS("udp", dstAddr.IP.String(), strconv.Itoa(dstAddr.Port), udpServer.config)
			if wsConn == nil {
				break
			}
			udpServer.wsConnCache.Store(dstAddr.String(), wsConn)
			go udpServer.forwardClient(wsConn, dstAddr)
		}
		wsConn.WriteMessage(websocket.BinaryMessage, data)
		log.Printf("[udp] client to remote %v:%v", dstAddr.IP.String(), strconv.Itoa(dstAddr.Port))
	}
	log.Printf("[udp] forward remote done")
}

func (udpServer *UDPServer) forwardClient(wsConn *websocket.Conn, dstAddr *net.UDPAddr) {
	defer wsConn.Close()
	bufCopy := make([]byte, BufferSize)
	for {
		wsConn.SetReadDeadline(time.Now().Add(60 * time.Second))
		_, buffer, err := wsConn.ReadMessage()
		if err != nil || err == io.EOF || len(buffer) == 0 {
			break
		}
		if header, ok := udpServer.dstAddrCache.Load(dstAddr.String()); ok {
			head := []byte(header.(string))
			headLength := len(head)
			copy(bufCopy[0:], head[0:headLength])
			copy(bufCopy[headLength:], buffer[0:])
			data := bufCopy[0 : headLength+len(buffer)]
			log.Printf("[udp] remote to client %v", udpServer.clientAddr)
			udpServer.serverConn.WriteToUDP(data, udpServer.clientAddr)
		}
	}
	log.Printf("[udp] forward client done")
}

func keepUDPAlive(tcpConn *net.TCPConn, done chan<- bool) {
	tcpConn.SetKeepAlive(true)
	buf := make([]byte, BufferSize)
	for {
		_, err := tcpConn.Read(buf[0:])
		if err != nil {
			break
		}
	}
	done <- true
}

func forwardUDP(udpConn *net.UDPConn, config config.Config) {
	udpServer := &UDPServer{serverConn: udpConn, config: config}
	udpServer.forwardRemote()
}

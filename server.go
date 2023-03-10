package pyritego

import (
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/mo-crystal/pyrite-go/utils"
)

type ClientData struct {
	Addr         *net.UDPAddr
	LastAccept   int64
	Sequence     int
	SequenceBuff map[int]chan *PrtPackage
}

type Server struct {
	listener    *net.UDPConn
	port        int
	sessionlen  int
	maxLifeTime int64
	router      map[string]func(PrtPackage) string
	timeout     time.Duration
	cdataMutex  sync.Mutex
	cdata       map[string]*ClientData
}

var (
	ErrServerUDPStartingFailed = errors.New("fail to start udp server")
)

//func processAlive(pkage PrtPackage)*PrtPackage

func NewServer(port int, maxTime int64, timeout time.Duration) (*Server, error) {

	var server Server
	server.port = port
	server.sessionlen = 16
	server.router = make(map[string]func(PrtPackage) string)
	server.cdata = make(map[string]*ClientData)
	server.maxLifeTime = maxTime
	server.timeout = timeout
	//server.router["prt-alive"] = processAlive
	return &server, nil
}

func (s *Server) AddRouter(identifier string, controller func(PrtPackage) string) bool {
	if strings.Index(identifier, "prt-") == 0 {
		return false
	}

	s.router[identifier] = controller
	return true
}

func (s *Server) GenerateSession() string {
	var ret string
	for {
		ret = utils.RandomString(s.sessionlen)
		s.cdataMutex.Lock()
		if _, ok := s.cdata[ret]; !ok {
			s.cdataMutex.Unlock()
			break
		}
		s.cdataMutex.Unlock()
	}
	return ret
}

func (s *Server) getSequence(session string) int {
	defer s.cdataMutex.Unlock()
	s.cdataMutex.Lock()
	s.cdata[session].Sequence += 1
	return s.cdata[session].Sequence - 1
}

func (s *Server) Tell(session string, identifier, body string) error {
	rBytes := PrtPackage{
		Session:    session,
		Identifier: identifier,
		sequence:   -1,
		Body:       body,
	}.ToBytes()
	if len(rBytes) > MAX_TRANSMIT_SIZE {
		return ErrContentOverflowed
	}

	s.listener.WriteToUDP(rBytes, s.cdata[session].Addr)
	return nil
}

// 向对方发送信息，并且期待 ACK
//
// 此函数会阻塞线程
func (s *Server) Promise(session string, identifier, body string) (string, error) {
	var response *PrtPackage
	var err error
	req := PrtPackage{
		Session:    session,
		Identifier: identifier,
		sequence:   s.getSequence(session),
		Body:       body,
	}

	reqBytes := req.ToBytes()
	if len(reqBytes) > MAX_TRANSMIT_SIZE {
		return "", ErrContentOverflowed
	}

	s.cdataMutex.Lock()
	s.listener.WriteToUDP(reqBytes, s.cdata[session].Addr)
	s.cdata[session].SequenceBuff[s.cdata[session].Sequence] = make(chan *PrtPackage)
	s.cdataMutex.Unlock()
	ch := make(chan bool)
	go Timer(s.timeout, ch, false)
	go func(err *error, ch chan bool) {
		defer func() { recover() }()
		response = <-s.cdata[session].SequenceBuff[s.cdata[session].Sequence]
		ch <- true
	}(&err, ch)

	ok := <-ch
	if !ok {
		return "", ErrTimeout
	}

	return response.Body, nil
}

func (s *Server) processAck(response *PrtPackage) {
	session := response.Session
	s.cdataMutex.Lock()
	ch, ok := s.cdata[session].SequenceBuff[response.sequence]
	s.cdataMutex.Unlock()
	if !ok {
		return
	}

	ch <- response
	close(ch)
	s.cdataMutex.Lock()
	delete(s.cdata[session].SequenceBuff, response.sequence)
	s.cdataMutex.Unlock()
}

func (s *Server) process(addr net.UDPAddr, recv []byte) {
	prtPack, err := CastToPrtPackage(recv)
	if err != nil {
		return
	}
	now := time.Now().UnixMicro()
	nowSession := prtPack.Session
	if prtPack.Session == "" {
		nowSession = s.GenerateSession()
		s.cdataMutex.Lock()
		s.cdata[nowSession] = &ClientData{
			Addr:         &addr,
			LastAccept:   now,
			Sequence:     0,
			SequenceBuff: make(map[int]chan *PrtPackage),
		}
		s.cdataMutex.Unlock()
	}

	if prtPack.Identifier == "prt-ack" {
		s.processAck(prtPack)
		return
	}

	f, ok := s.router[prtPack.Identifier]
	if !ok {
		return
	}

	resp := f(*prtPack)
	if resp == "" {
		return
	}

	s.cdataMutex.Lock()
	s.listener.WriteToUDP(PrtPackage{
		Session:    nowSession,
		Identifier: "prt-ack",
		sequence:   prtPack.sequence,
		Body:       resp,
	}.ToBytes(), s.cdata[nowSession].Addr)
	s.cdataMutex.Unlock()
}

func (s *Server) Start() error {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("0.0.0.0"), Port: s.port})
	if err != nil {
		return ErrServerUDPStartingFailed
	}

	s.listener = listener
	recvBuf := make([]byte, MAX_TRANSMIT_SIZE)
	var n int
	var addr *net.UDPAddr
	go s.GC()
	for {
		n, addr, err = listener.ReadFromUDP(recvBuf)
		if err != nil || n == 0 {
			panic("invalid msg recved")
		}

		slice := make([]byte, n)
		copy(slice, recvBuf)
		go s.process(*addr, slice)
	}
}

func (s *Server) GC() {
	for {
		nowTime := time.Now().UnixMicro()
		for k, v := range s.cdata {
			if nowTime-v.LastAccept >= s.maxLifeTime {
				delete(s.cdata, k)
			}
		}
		endTime := time.Now().UnixMicro()
		if endTime-nowTime-s.maxLifeTime > 0 {
			sleepTime := time.Duration(endTime - nowTime - s.maxLifeTime)
			time.Sleep(sleepTime)
		}
	}
}

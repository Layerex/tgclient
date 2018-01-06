package mtproto

import (
	"crypto/sha256"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/ansel1/merry"
)

//go:generate go run scheme/generate_tl_schema.go scheme/tl-schema-71.json tl_schema.go
//go:generate gofmt -w tl_schema.go

var ErrNoSessionData = merry.New("no session data")

type SessionInfo struct {
	AuthKey     []byte
	AuthKeyHash []byte
	ServerSalt  []byte
	Addr        string
	sessionId   int64
}

type SessionStore interface {
	Save(*SessionInfo) error
	Load(*SessionInfo) error
}

type SessFileStore struct {
	fpath string
}

func (s *SessFileStore) Save(sess *SessionInfo) (err error) {
	f, err := os.Create(s.fpath)
	if err != nil {
		return merry.Wrap(err)
	}
	defer f.Close()

	b := NewEncodeBuf(1024)
	b.StringBytes(sess.AuthKey)
	b.StringBytes(sess.AuthKeyHash)
	b.StringBytes(sess.ServerSalt)
	b.String(sess.Addr)

	_, err = f.Write(b.buf)
	if err != nil {
		return merry.Wrap(err)
	}
	return nil
}

func (s *SessFileStore) Load(sess *SessionInfo) error {
	f, err := os.Open(s.fpath)
	if os.IsNotExist(err) {
		return ErrNoSessionData.Here()
	}
	if err != nil {
		return merry.Wrap(err)
	}
	defer f.Close()

	b := make([]byte, 1024*4)
	_, err = f.Read(b)
	if err != nil {
		return merry.Wrap(err)
	}

	d := NewDecodeBuf(b)
	sess.AuthKey = d.StringBytes()
	sess.AuthKeyHash = d.StringBytes()
	sess.ServerSalt = d.StringBytes()
	sess.Addr = d.String()

	if d.err != nil {
		return merry.Wrap(d.err)
	}
	return nil
}

type AppConfig struct {
	AppID          int32
	AppHash        string
	AppVersion     string
	DeviceModel    string
	SystemVersion  string
	SystemLangCode string
	LangPack       string
	LangCode       string
}

type MTProto struct {
	sessionStore SessionStore
	session      *SessionInfo
	appCfg       *AppConfig
	conn         *net.TCPConn

	queueSend chan packetToSend
	stopSend  chan struct{}
	stopRead  chan struct{}
	stopPing  chan struct{}
	allDone   chan struct{}

	mutex           *sync.Mutex
	encryptionReady bool
	lastSeqNo       int32
	msgsIdToAck     map[int64]packetToSend
	msgsIdToResp    map[int64]chan TL
	seqNo           int32
	msgId           int64

	dclist map[int32]string
}

type packetToSend struct {
	msg  TL
	resp chan TL
}

func NewMTProto(appID int32, appHash string) (*MTProto, error) {
	// getting exec directory
	ex, err := os.Executable()
	if err != nil {
		return nil, merry.Wrap(err)
	}
	exPath := filepath.Dir(ex)

	// base setup
	m := &MTProto{
		sessionStore: &SessFileStore{exPath + "/tg.session"},
		session:      &SessionInfo{},
		appCfg: &AppConfig{
			AppID:          appID,
			AppHash:        appHash,
			AppVersion:     "0.0.4",
			DeviceModel:    "Unknown",
			SystemVersion:  runtime.GOOS + "/" + runtime.GOARCH,
			SystemLangCode: "en",
			LangPack:       "",
			LangCode:       "en",
		},
	}

	// loading session
	err = m.sessionStore.Load(m.session)
	if merry.Is(err, ErrNoSessionData) { //no data
		m.session.Addr = "149.154.167.50:443" //"149.154.167.40"
		m.encryptionReady = false
	} else if err == nil { //got saved session
		m.encryptionReady = true
	} else {
		return nil, merry.Wrap(err)
	}

	rand.Seed(time.Now().UnixNano())
	m.session.sessionId = rand.Int63()
	return m, nil
}

func (m *MTProto) Connect() error {
	// connecting
	tcpAddr, err := net.ResolveTCPAddr("tcp", m.session.Addr)
	if err != nil {
		return merry.Wrap(err)
	}
	m.conn, err = net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		return merry.Wrap(err)
	}
	_, err = m.conn.Write([]byte{0xef})
	if err != nil {
		return merry.Wrap(err)
	}

	// getting new authKey if need
	if !m.encryptionReady {
		if err = m.makeAuthKey(); err != nil {
			return merry.Wrap(err)
		}
		if err := m.sessionStore.Save(m.session); err != nil {
			return merry.Wrap(err)
		}
		m.encryptionReady = true
	}

	// starting goroutines
	m.queueSend = make(chan packetToSend, 64)
	m.stopSend = make(chan struct{}, 1)
	m.stopRead = make(chan struct{}, 1)
	m.stopPing = make(chan struct{}, 1)
	m.allDone = make(chan struct{}, 3)
	m.msgsIdToAck = make(map[int64]packetToSend)
	m.msgsIdToResp = make(map[int64]chan TL)
	m.mutex = &sync.Mutex{}
	go m.sendRoutine()
	go m.readRoutine()

	// getting connection configs
	x := m.SendSync(TL_invokeWithLayer{
		layer,
		TL_initConnection{
			m.appCfg.AppID,
			m.appCfg.DeviceModel,
			m.appCfg.SystemVersion,
			m.appCfg.AppVersion,
			m.appCfg.SystemLangCode,
			m.appCfg.LangPack,
			m.appCfg.LangCode,
			TL_help_getConfig{},
		},
	})
	if cfg, ok := x.(TL_config); ok {
		m.dclist = make(map[int32]string, 5)
		for _, v := range cfg.DcOptions {
			v := v.(TL_dcOption)
			m.dclist[v.ID] = fmt.Sprintf("%s:%d", v.IpAddress, v.Port)
		}
	} else {
		return WrongRespError(x)
	}

	// starting keepalive pinging
	go m.pingRoutine()
	return nil
}

func (m *MTProto) reconnect(newaddr string) error {
	// stopping routines
	m.stopPing <- struct{}{}
	m.stopSend <- struct{}{}
	m.stopRead <- struct{}{}
	<-m.allDone
	<-m.allDone
	<-m.allDone
	close(m.stopPing)
	close(m.stopSend)
	close(m.stopRead)

	// closing send queue
	close(m.queueSend)

	// closing connection
	if err := m.conn.Close(); err != nil {
		return merry.Wrap(err)
	}

	// renewing connection
	m.encryptionReady = false
	m.session.Addr = newaddr
	if err := m.Connect(); err != nil {
		return merry.Wrap(err)
	}
	return nil
}

func (m *MTProto) SendSync(msg TL) TL {
	resp := make(chan TL, 1)
	m.queueSend <- packetToSend{msg, resp}
	return <-resp
}

func (m *MTProto) Auth() error {
	var phonenumber string
	fmt.Print("Enter phone number: ")
	fmt.Scanf("%s", &phonenumber)

	var authSentCode TL_auth_sentCode
	flag := true
	for flag {
		x := m.SendSync(TL_auth_sendCode{
			CurrentNumber: TL_boolTrue{},
			PhoneNumber:   phonenumber,
			ApiID:         m.appCfg.AppID,
			ApiHash:       m.appCfg.AppHash,
		})
		switch x.(type) {
		case TL_auth_sentCode:
			authSentCode = x.(TL_auth_sentCode)
			flag = false
		case TL_rpc_error:
			x := x.(TL_rpc_error)
			if x.error_code != TL_ErrSeeOther {
				return WrongRespError(x)
			}
			var newDc int32
			n, _ := fmt.Sscanf(x.error_message, "PHONE_MIGRATE_%d", &newDc)
			if n != 1 {
				n, _ := fmt.Sscanf(x.error_message, "NETWORK_MIGRATE_%d", &newDc)
				if n != 1 {
					return fmt.Errorf("RPC error_string: %s", x.error_message)
				}
			}

			newDcAddr, ok := m.dclist[newDc]
			if !ok {
				return fmt.Errorf("Wrong DC index: %d", newDc)
			}
			if err := m.reconnect(newDcAddr); err != nil {
				return merry.Wrap(err)
			}
		default:
			return WrongRespError(x)
		}
	}

	var code string
	fmt.Print("Enter code: ")
	fmt.Scanf("%s", &code)

	//if authSentCode.Phone_registered
	x := m.SendSync(TL_auth_signIn{phonenumber, authSentCode.PhoneCodeHash, code})
	if IsError(x, "SESSION_PASSWORD_NEEDED") {
		x = m.SendSync(TL_account_getPassword{})
		accPasswd, ok := x.(TL_account_password)
		if !ok {
			return WrongRespError(x)
		}

		var passwd string
		fmt.Print("Enter password: ")
		fmt.Scanf("%s", &passwd)

		salt := string(accPasswd.CurrentSalt)
		hash := sha256.Sum256([]byte(salt + passwd + salt))
		x = m.SendSync(TL_auth_checkPassword{hash[:]})
		if _, ok := x.(TL_rpc_error); ok {
			return WrongRespError(x)
		}
	}
	auth, ok := x.(TL_auth_authorization)
	if !ok {
		return fmt.Errorf("RPC: %#v", x)
	}
	userSelf := auth.User.(TL_user)
	fmt.Printf("Signed in: id %d name <%s %s>\n", userSelf.ID, userSelf.FirstName, userSelf.LastName)
	return nil
}

func (m *MTProto) GetContacts() error {
	resp := make(chan TL, 1)
	m.queueSend <- packetToSend{TL_contacts_getContacts{0}, resp}
	x := <-resp
	list, ok := x.(TL_contacts_contacts)
	if !ok {
		return fmt.Errorf("RPC: %#v", x)
	}

	contacts := make(map[int32]TL_user)
	for _, v := range list.Users {
		if v, ok := v.(TL_user); ok {
			contacts[v.ID] = v
		}
	}
	fmt.Printf(
		"\033[33m\033[1m%10s    %10s    %-30s    %-20s\033[0m\n",
		"id", "mutual", "name", "username",
	)
	for _, v := range list.Contacts {
		v := v.(TL_contact)
		fmt.Printf(
			"%10d    %10t    %-30s    %-20s\n",
			v.UserID,
			toBool(v.Mutual),
			fmt.Sprintf("%s %s", contacts[v.UserID].FirstName, contacts[v.UserID].LastName),
			contacts[v.UserID].Username,
		)
	}

	return nil
}

/*func (m *MTProto) SendMessage(user_id int32, msg string) error {
	resp := make(chan TL, 1)
	m.queueSend <- packetToSend{
		TL_messages_sendMessage{
			TL_inputPeerContact{user_id},
			msg,
			rand.Int63(),
		},
		resp,
	}
	x := <-resp
	_, ok := x.(TL_messages_sentMessage)
	if !ok {
		return fmt.Errorf("RPC: %#v", x)
	}

	return nil
}*/

func (m *MTProto) pingRoutine() {
	for {
		select {
		case <-m.stopPing:
			m.allDone <- struct{}{}
			return
		case <-time.After(60 * time.Second):
			m.queueSend <- packetToSend{TL_ping{0xCADACADA}, nil}
		}
	}
}

func (m *MTProto) sendRoutine() {
	for x := range m.queueSend {
		//DEBUG fmt.Printf("Senging: %T %v\n", x.msg, x.msg)
		err := m.send(x.msg, x.resp)
		if err != nil {
			log.Fatal(merry.Details(err)) //TODO: better handling
		}
	}

	m.allDone <- struct{}{}
}

func (m *MTProto) readRoutine() {
	for {
		data, err := m.read(m.stopRead)
		//DEBUG fmt.Printf("Read: %T %v\n", data, data)
		if err != nil {
			log.Fatal(merry.Details(err)) //TODO: better handling
		}
		if data == nil { //nil data means reconnection, not very good TODO
			m.allDone <- struct{}{}
			return
		}

		x := m.process(m.msgId, m.seqNo, data)
		fmt.Printf("recv: %T %+v\n", x, x)
	}

}

func (m *MTProto) process(msgId int64, seqNo int32, data interface{}) interface{} {
	switch data.(type) {
	case TL_msg_container:
		data := data.(TL_msg_container).items //TODO
		for _, v := range data {
			m.process(v.msg_id, v.seq_no, v.data)
		}

	case TL_bad_server_salt:
		data := data.(TL_bad_server_salt)
		m.session.ServerSalt = data.new_server_salt
		_ = m.sessionStore.Save(m.session) //TODO
		m.mutex.Lock()
		for k, v := range m.msgsIdToAck {
			delete(m.msgsIdToAck, k)
			m.queueSend <- v //may lock here, TODO
		}
		m.mutex.Unlock()

	case TL_new_session_created:
		data := data.(TL_new_session_created)
		m.session.ServerSalt = data.server_salt
		_ = m.sessionStore.Save(m.session) //TODO

	case TL_ping:
		data := data.(TL_ping)
		m.queueSend <- packetToSend{TL_pong{msgId, data.ping_id}, nil}

	case TL_pong:
		// (ignore) TODO

	case TL_msgs_ack:
		data := data.(TL_msgs_ack)
		m.mutex.Lock()
		for _, v := range data.msgIds {
			delete(m.msgsIdToAck, v)
		}
		m.mutex.Unlock()

	case TL_rpc_result:
		data := data.(TL_rpc_result)
		//DEBUG fmt.Println(msgId, seqNo, data)
		//DEBUG fmt.Printf("%v %T\n", data, data)
		x := m.process(msgId, seqNo, data.obj)
		m.mutex.Lock()
		v, ok := m.msgsIdToResp[data.req_msg_id]
		if ok {
			v <- x.(TL)
			close(v)
			delete(m.msgsIdToResp, data.req_msg_id)
		}
		delete(m.msgsIdToAck, data.req_msg_id)
		m.mutex.Unlock()

	default:
		return data

	}

	if (seqNo & 1) == 1 {
		m.queueSend <- packetToSend{TL_msgs_ack{[]int64{msgId}}, nil}
	}

	return nil
}

package rpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	hadoop "github.com/raksh93/hdfs/internal/protocol/hadoop_common"
	"github.com/golang/protobuf/proto"
	krb "gopkg.in/jcmturner/gokrb5.v5/client"
)

const (
	rpcVersion            byte = 0x09
	serviceClass          byte = 0x0
	noneAuthProtocol      byte = 0x0
	saslAuthProtocol      byte = 0xdf
	protocolClass              = "org.apache.hadoop.hdfs.protocol.ClientProtocol"
	protocolClassVersion       = 1
	handshakeCallID            = -3
	standbyExceptionClass      = "org.apache.hadoop.ipc.StandbyException"
)

const backoffDuration = time.Second * 5

// NamenodeConnection represents an open connection to a namenode.
type NamenodeConnection struct {
	ClientID   []byte
	ClientName string
	User       string

	currentRequestID int32

	kerberosClient               *krb.Client
	kerberosServicePrincipleName string
	kerberosRealm                string

	dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)
	conn     net.Conn
	host     *namenodeHost
	hostList []*namenodeHost

	reqLock sync.Mutex
}

// NamenodeConnectionOptions represents the configurable options available
// for a NamenodeConnection.
type NamenodeConnectionOptions struct {
	// Addresses specifies the namenode(s) to connect to.
	Addresses []string
	// User specifies which HDFS user the client will act as. It is required
	// unless kerberos authentication is enabled, in which case it will be
	// determined from the provided credentials if empty.
	User string
	// DialFunc is used to connect to the datanodes. If nil, then
	// (&net.Dialer{}).DialContext is used.
	DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)
	// KerberosClient is used to connect to kerberized HDFS clusters. If provided,
	// the NamenodeConnection will always mutually athenticate when connecting
	// to the namenode(s).
	KerberosClient *krb.Client
	// KerberosServicePrincipleName specifiesthe Service Principle Name
	// (<SERVICE>/<FQDN>) for the namenode(s). Like in the
	// dfs.namenode.kerberos.principal property of core-site.xml, the special
	// string '_HOST' can be substituted for the hostname in a multi-namenode
	// setup (for example: 'nn/_HOST@EXAMPLE.COM'). It is required if
	// KerberosClient is provided.
	KerberosServicePrincipleName string
}

type namenodeHost struct {
	address     string
	lastError   error
	lastErrorAt time.Time
}

// NewNamenodeConnectionWithOptions creates a new connection to a namenode with
// the given options and performs an initial handshake.
func NewNamenodeConnection(options NamenodeConnectionOptions) (*NamenodeConnection, error) {
	// Build the list of hosts to be used for failover.
	hostList := make([]*namenodeHost, len(options.Addresses))
	for i, addr := range options.Addresses {
		hostList[i] = &namenodeHost{address: addr}
	}

	var user, realm string
	user = options.User
	if user == "" {
		if options.KerberosClient != nil {
			creds := options.KerberosClient.Credentials
			user = creds.Username
			realm = creds.Realm
		} else {
			return nil, errors.New("user not specified")
		}
	}

	// The ClientID is reused here both in the RPC headers (which requires a
	// "globally unique" ID) and as the "client name" in various requests.
	clientId := newClientID()
	c := &NamenodeConnection{
		ClientID:   clientId,
		ClientName: "go-hdfs-" + string(clientId),
		User:       user,

		kerberosClient:               options.KerberosClient,
		kerberosServicePrincipleName: options.KerberosServicePrincipleName,
		kerberosRealm:                realm,

		dialFunc: options.DialFunc,
		hostList: hostList,
	}

	err := c.resolveConnection()
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (c *NamenodeConnection) resolveConnection() error {
	if c.conn != nil {
		return nil
	}

	var err error
	if c.host != nil {
		err = c.host.lastError
	}

	for _, host := range c.hostList {
		if host.lastErrorAt.After(time.Now().Add(-backoffDuration)) {
			continue
		}

		if c.dialFunc == nil {
			c.dialFunc = (&net.Dialer{}).DialContext
		}

		c.host = host
		c.conn, err = c.dialFunc(context.Background(), "tcp", host.address)
		if err != nil {
			c.markFailure(err)
			continue
		}

		err = c.doNamenodeHandshake()
		if err != nil {
			c.markFailure(err)
			continue
		}

		break
	}

	if c.conn == nil {
		return fmt.Errorf("no available namenodes: %s", err)
	}

	return nil
}

func (c *NamenodeConnection) markFailure(err error) {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.host.lastError = err
	c.host.lastErrorAt = time.Now()
}

// Execute performs an rpc call. It does this by sending req over the wire and
// unmarshaling the result into resp.
func (c *NamenodeConnection) Execute(method string, req proto.Message, resp proto.Message) error {
	c.reqLock.Lock()
	defer c.reqLock.Unlock()

	c.currentRequestID++

	for {
		err := c.resolveConnection()
		if err != nil {
			return err
		}

		err = c.writeRequest(method, req)
		if err != nil {
			c.markFailure(err)
			continue
		}

		err = c.readResponse(method, resp)
		if err != nil {
			// Only retry on a standby exception.
			if nerr, ok := err.(*NamenodeError); ok && nerr.exception == standbyExceptionClass {
				c.markFailure(err)
				continue
			}

			return err
		}

		break
	}

	return nil
}

// RPC definitions

// A request packet:
// +-----------------------------------------------------------+
// |  uint32 length of the next three parts                    |
// +-----------------------------------------------------------+
// |  varint length + RpcRequestHeaderProto                    |
// +-----------------------------------------------------------+
// |  varint length + RequestHeaderProto                       |
// +-----------------------------------------------------------+
// |  varint length + Request                                  |
// +-----------------------------------------------------------+
func (c *NamenodeConnection) writeRequest(method string, req proto.Message) error {
	rrh := newRPCRequestHeader(c.currentRequestID, c.ClientID)
	rh := newRequestHeader(method)

	reqBytes, err := makeRPCPacket(rrh, rh, req)
	if err != nil {
		return err
	}

	_, err = c.conn.Write(reqBytes)
	return err
}

// A response from the namenode:
// +-----------------------------------------------------------+
// |  uint32 length of the next two parts                      |
// +-----------------------------------------------------------+
// |  varint length + RpcResponseHeaderProto                   |
// +-----------------------------------------------------------+
// |  varint length + Response                                 |
// +-----------------------------------------------------------+
func (c *NamenodeConnection) readResponse(method string, resp proto.Message) error {
	rrh := &hadoop.RpcResponseHeaderProto{}
	err := readRPCPacket(c.conn, rrh, resp)
	if err != nil {
		return err
	} else if int32(rrh.GetCallId()) != c.currentRequestID {
		return errors.New("unexpected sequence number")
	} else if rrh.GetStatus() != hadoop.RpcResponseHeaderProto_SUCCESS {
		return &NamenodeError{
			method:    method,
			message:   rrh.GetErrorMsg(),
			code:      int(rrh.GetErrorDetail()),
			exception: rrh.GetExceptionClassName(),
		}
	}

	return nil
}

// A handshake packet:
// +-----------------------------------------------------------+
// |  Header, 4 bytes ("hrpc")                                 |
// +-----------------------------------------------------------+
// |  Version, 1 byte (default verion 0x09)                    |
// +-----------------------------------------------------------+
// |  RPC service class, 1 byte (0x00)                         |
// +-----------------------------------------------------------+
// |  Auth protocol, 1 byte (Auth method None = 0x00)          |
// +-----------------------------------------------------------+
//
//  If the auth protocol is something other than 'none', the authentication
//  handshake happens here. Otherwise, everything can be sent as one packet.
//
// +-----------------------------------------------------------+
// |  uint32 length of the next two parts                      |
// +-----------------------------------------------------------+
// |  varint length + RpcRequestHeaderProto                    |
// +-----------------------------------------------------------+
// |  varint length + IpcConnectionContextProto                |
// +-----------------------------------------------------------+
func (c *NamenodeConnection) doNamenodeHandshake() error {
	authProtocol := noneAuthProtocol
	kerberos := false
	if c.kerberosClient != nil {
		authProtocol = saslAuthProtocol
		kerberos = true
	}

	rpcHeader := []byte{
		0x68, 0x72, 0x70, 0x63, // "hrpc"
		rpcVersion, serviceClass, authProtocol,
	}

	_, err := c.conn.Write(rpcHeader)
	if err != nil {
		return err
	}

	if kerberos {
		err = c.doKerberosHandshake()
		if err != nil {
			return fmt.Errorf("SASL handshake: %s", err)
		}

		// Reset the sequence number here, since we set it to -33 for the SASL bits.
		c.currentRequestID = 0
	}

	rrh := newRPCRequestHeader(handshakeCallID, c.ClientID)
	cc := newConnectionContext(c.User, c.kerberosRealm)
	packet, err := makeRPCPacket(rrh, cc)
	if err != nil {
		return err
	}

	_, err = c.conn.Write(packet)
	return err
}

// Close terminates all underlying socket connections to remote server.
func (c *NamenodeConnection) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

func newRPCRequestHeader(id int32, clientID []byte) *hadoop.RpcRequestHeaderProto {
	return &hadoop.RpcRequestHeaderProto{
		RpcKind:  hadoop.RpcKindProto_RPC_PROTOCOL_BUFFER.Enum(),
		RpcOp:    hadoop.RpcRequestHeaderProto_RPC_FINAL_PACKET.Enum(),
		CallId:   proto.Int32(id),
		ClientId: clientID,
	}
}

func newRequestHeader(methodName string) *hadoop.RequestHeaderProto {
	return &hadoop.RequestHeaderProto{
		MethodName:                 proto.String(methodName),
		DeclaringClassProtocolName: proto.String(protocolClass),
		ClientProtocolVersion:      proto.Uint64(uint64(protocolClassVersion)),
	}
}

func newConnectionContext(user, kerberosRealm string) *hadoop.IpcConnectionContextProto {
	if kerberosRealm != "" {
		user = user + "@" + kerberosRealm
	}

	return &hadoop.IpcConnectionContextProto{
		UserInfo: &hadoop.UserInformationProto{
			EffectiveUser: proto.String(user),
		},
		Protocol: proto.String(protocolClass),
	}
}

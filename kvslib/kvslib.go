package kvslib

import (
	"errors"
	"net"
	"net/rpc"
	"sync"
	"sync/atomic"
	"time"

	"cs.ubc.ca/cpsc416/kvraft/util"

	"github.com/DistributedClocks/tracing"
)

// Actions to be recorded by kvslib (as part of ktrace, put trace, get trace):

type KvslibStart struct {
	ClientId string
}

type KvslibStop struct {
	ClientId string
}

type Put struct {
	ClientId string
	OpId     uint32
	Key      string
	Value    string
}

type PutResultRecvd struct {
	OpId uint32
	Key  string
}

type Get struct {
	ClientId string
	OpId     uint32
	Key      string
}

type GetResultRecvd struct {
	OpId  uint32
	Key   string
	Value string
}

type LeaderReq struct {
	ClientId string
}

type LeaderResRecvd struct {
	ClientId string
	ServerId uint8
}

type LeaderNodeUpdateRecvd struct {
	ClientId string
	ServerId uint8
}

// NotifyChannel is used for notifying the client about a mining result.
type NotifyChannel chan ResultStruct

type ResultStruct struct {
	OpId   uint32
	Result string
}

type KVS struct {
	notifyCh NotifyChannel
	// add more KVS instance state here.
	clientId string

	// opId for the client
	opId uint32

	// tracer
	kTracer *tracing.Tracer

	// traces
	ktrace   *tracing.Trace
	putTrace *tracing.Trace
	getTrace *tracing.Trace

	// trackings
	resRecvd map[uint32]interface{}

	// IP & ports

	//leaderServerIPPort    string

	// putListenerIPPort   string
	coordListenerIPPort string

	// leader ports

	leaderNodeIPPort string

	// TCP Addrs
	lnLocalTCPAddr *net.TCPAddr

	// RPCs:
	coordClientRPC *rpc.Client
	//hsClientRPC    *rpc.Client
	leaderClientRPC *rpc.Client

	// Listeners:
	// putTCPListener *net.TCPListener
	// tsRPCListener *rpc.Server
	serverFailListener *net.TCPListener

	// struct with channels:
	cnl *CoordListener // coord notification listener

	// sync
	allResRecvd chan bool
	allErrRecvd chan bool

	// mutex
	reqLock sync.Mutex

	reqQueue               chan Req
	leaderNodeLock         sync.Mutex
	leaderReconfiguredDone chan bool

	chCapacity int
}

type CCoordGetLeaderNodeArg struct {
	ClientInfo
	Token tracing.TracingToken
}
type CCoordGetLeaderNodeReply struct {
	ServerId     uint8
	ServerIpPort string
	Token        tracing.TracingToken
}

type ClientInfo struct {
	ClientId           string
	CoordAPIListenAddr string
}

type GetRequest struct {
	ClientId string
	OpId     uint32
	Key      string
	Token    tracing.TracingToken
}

type GetResponse struct {
	ClientId string
	OpId     uint32
	Key      string
	Value    string
	Token    tracing.TracingToken
}

type PutRequest struct {
	ClientId string
	OpId     uint32
	//NewGetGId       uint64
	Key   string
	Value string
	//PutListenerAddr string
	Token tracing.TracingToken
}

type PutResponse struct {
	ClientId string
	OpId     uint32
	Key      string
	Value    string
	Token    tracing.TracingToken
}

type Req struct {
	pq     PutRequest
	gq     GetRequest
	kind   string // kind is either PUT or GET or STOP
	tracer *tracing.Tracer
}

type ServerListener struct {
	PutResChan chan PutResponse
}

type CoordListener struct {
	ServerFailChan chan ServerFail
	// Kvs            *KVS
}

type ReqQueue struct {
	QueueLock sync.Mutex
	// queue type?
}

type ServerFail struct {
	ServerPosition  ServerPos
	NewServerIpPort string
}

type ServerPos uint8

const (
	StopServer           = 100 // a server id is guanranteed to be unused
	Leader     ServerPos = iota
)

type AddCNLReq struct {
	ClientId            string
	CoordListenerIpPort string
}

type NewLeaderStruct struct {
	Ip        string
	ServerNum uint8
	Token     tracing.TracingToken
}

type NewLeaderChangedStruct struct {
	Token tracing.TracingToken
}

func NewKVS() *KVS {
	return &KVS{
		notifyCh: nil,
	}
}

// Start Starts the instance of KVS to use for connecting to the system with the given coord's IP:port.
// The returned notify-channel channel must have capacity ChCapacity and must be used by kvslib to deliver
// all get/put output notifications. ChCapacity determines the concurrency
// factor at the client: the client will never have more than ChCapacity number of operations outstanding (pending concurrently) at any one time.
// If there is an issue with connecting to the coord, this should return an appropriate err value, otherwise err should be set to nil.
func (d *KVS) Start(localTracer *tracing.Tracer, clientId string, coordIPPort string, localCoordIPPort string, localLeaderNodeIPPort string, chCapacity int) (NotifyChannel, error) {
	var err error
	d.reqQueue = make(chan Req, chCapacity)
	d.chCapacity = chCapacity
	d.clientId = clientId
	d.ktrace = localTracer.CreateTrace()
	d.ktrace.GenerateToken()
	d.resRecvd = make(map[uint32]interface{})
	d.allResRecvd = make(chan bool)
	d.allErrRecvd = make(chan bool)
	d.kTracer = localTracer

	/*Connect to Coord as client*/
	coordLAddr, coordLAddrErr := net.ResolveTCPAddr("tcp", localCoordIPPort)
	if err = checkCriticalErr(coordLAddrErr, "an error resolving addr for localCoordIPPort: "); err != nil {
		return nil, err
	}
	coordAddr, coordAddrErr := net.ResolveTCPAddr("tcp", coordIPPort)
	if err = checkCriticalErr(coordAddrErr, "an error resolving addr for coordIPPort: "); err != nil {
		return nil, err
	}
	cTCPConn, cTCPConnErr := net.DialTCP("tcp", coordLAddr, coordAddr)
	if err = checkCriticalErr(cTCPConnErr, "an error dialing tcp from kvslib to coord: "); err != nil {
		return nil, err
	}
	cTCPConn.SetLinger(0)
	coordClient := rpc.NewClient(cTCPConn)
	d.coordClientRPC = coordClient
	d.ktrace.RecordAction(KvslibStart{clientId})

	/*Start coord listener for server failure*/
	coordListenerIPPort := coordLAddr.IP.String() + ":0"
	coordListenerAddr, coordListenerAddrErr := net.ResolveTCPAddr("tcp", coordListenerIPPort)
	if err = checkCriticalErr(coordListenerAddrErr, "error finding a random port for coord listener: "); err != nil {
		return nil, err
	}
	coordTCPListener, coordTCPListenerErr := net.ListenTCP("tcp", coordListenerAddr)
	if err = checkCriticalErr(coordTCPListenerErr, "an error setting up server failure listener in kvslib: "); err != nil {
		return nil, err
	}
	d.coordListenerIPPort = coordTCPListener.Addr().String()

	d.serverFailListener = coordTCPListener
	cnl := &CoordListener{make(chan ServerFail)}
	d.cnl = cnl
	d.cnl.ServerFailChan = make(chan ServerFail, 16)
	coordRPCListener := rpc.NewServer()
	coordRegErr := coordRPCListener.Register(d)
	if err = checkCriticalErr(coordRegErr, "an error registering cnl for coordRPCListener in kvslib: "); err != nil {
		return nil, err
	}
	go coordRPCListener.Accept(coordTCPListener)

	/*Get Leader Server Addr from Coord*/
	d.ktrace.RecordAction(LeaderReq{clientId})
	var leaderNodeReq CCoordGetLeaderNodeArg = CCoordGetLeaderNodeArg{ClientInfo{clientId, d.coordListenerIPPort}, d.ktrace.GenerateToken()}
	var leaderNodeRes CCoordGetLeaderNodeReply
	lnReqErr := coordClient.Call("ClientLearnServers.GetLeaderNode", leaderNodeReq, &leaderNodeRes)
	if err = checkCriticalErr(lnReqErr, "an error requesting leader node from coord: "); err != nil {
		return nil, err
	}
	d.ktrace = localTracer.ReceiveToken(leaderNodeRes.Token)
	d.ktrace.RecordAction(LeaderResRecvd{clientId, leaderNodeRes.ServerId})
	d.leaderNodeIPPort = leaderNodeRes.ServerIpPort

	/*Connect to Leader*/
	lnLAddr, lnLAddrErr := net.ResolveTCPAddr("tcp", localLeaderNodeIPPort)
	if err = checkCriticalErr(lnLAddrErr, "an error resolving addr for localLeaderNodeIPPort: "); err != nil {
		return nil, err
	}
	d.lnLocalTCPAddr = lnLAddr
	lnAddr, lnAddrErr := net.ResolveTCPAddr("tcp", d.leaderNodeIPPort)
	if err = checkCriticalErr(lnAddrErr, "an error resolving addr for leaderNodeIPPort: "); err != nil {
		return nil, err
	}
	lnTCPConn, lnTCPConnErr := net.DialTCP("tcp", lnLAddr, lnAddr)
	if err = checkCriticalErr(lnTCPConnErr, "an error dialing tcp from kvslib to leader node: "); err != nil {
		return nil, err
	}
	lnTCPConn.SetLinger(0)
	lnClient := rpc.NewClient(lnTCPConn)
	d.leaderClientRPC = lnClient

	d.notifyCh = make(NotifyChannel, chCapacity)
	d.opId = 0
	go coordRPCListener.Accept(coordTCPListener)
	go d.sender()
	go d.handleFailure()
	return d.notifyCh, nil
}

// Get non-blocking request from the client to make a get call for a given key.
// In case there is an underlying issue (for example, servers/coord cannot be reached),
// this should return an appropriate err value, otherwise err should be set to nil. Note that this call is non-blocking.
// The returned value must be delivered asynchronously to the client via the notify-channel channel returned in the Start call.
// The value opId is used to identify this request and associate the returned value with this request.
func (d *KVS) Get(tracer *tracing.Tracer, clientId string, key string) (uint32, error) {
	atomic.AddUint32(&d.opId, 1)
	req := Req{
		gq: GetRequest{
			ClientId: clientId,
			OpId:     d.opId,
			Key:      key,
			Token:    tracing.TracingToken{}, // not used
		},
		kind:   "GET",
		tracer: tracer,
	}
	d.reqQueue <- req

	return d.opId, nil
}

// Put non-blocking request from the client to update the value associated with a key.
// In case there is an underlying issue (for example, the servers/coord cannot be reached),
// this should return an appropriate err value, otherwise err should be set to nil. Note that this call is non-blocking.
// The value opId is used to identify this request and associate the returned value with this request.
// The returned value must be delivered asynchronously via the notify-channel channel returned in the Start call.
func (d *KVS) Put(tracer *tracing.Tracer, clientId string, key string, value string) (uint32, error) {
	atomic.AddUint32(&d.opId, 1)
	req := Req{PutRequest{clientId, d.opId, key, value, tracing.TracingToken{}}, GetRequest{}, "PUT", tracer}
	d.reqQueue <- req

	return d.opId, nil

}

func (d *KVS) handleFailure() {
	d.leaderReconfiguredDone = make(chan bool, 256) // TODO maybe a bigger buffer is needed
	for {
		serverFailure := <-d.cnl.ServerFailChan
		if serverFailure.ServerPosition == Leader {
			d.leaderNodeLock.Lock()
			/* Close old client connection to HS */
			d.leaderNodeIPPort = serverFailure.NewServerIpPort
			hsClientRPCCloseErr := d.leaderClientRPC.Close()
			checkWarning(hsClientRPCCloseErr, "error closing the RPC client for leader server(HS) upon receiving failed HS msg in handlGet: ")

			/* Get new Leader server from coord */
			d.ktrace.RecordAction(LeaderReq{d.clientId})
			var leaderNodeReq CCoordGetLeaderNodeArg = CCoordGetLeaderNodeArg{ClientInfo{d.clientId, d.coordListenerIPPort}, d.ktrace.GenerateToken()}
			var leaderNodeRes CCoordGetLeaderNodeReply
			hsReqErr := d.coordClientRPC.Call("ClientLearnServers.GetLeaderNode", leaderNodeReq, &leaderNodeRes)
			if hsReqErr != nil {
				checkWarning(hsReqErr, "error requesting leader server from coord: ")
				continue
			}
			d.ktrace = d.kTracer.ReceiveToken(leaderNodeRes.Token)
			d.ktrace.RecordAction(LeaderResRecvd{d.clientId, leaderNodeRes.ServerId})
			d.leaderNodeIPPort = leaderNodeRes.ServerIpPort

			/* Start new client connection to HS */
			newHSAddr, newHSAddrErr := net.ResolveTCPAddr("tcp", d.leaderNodeIPPort)
			checkWarning(newHSAddrErr, "error resolving newHSAddr upon receiving failed HS msg in handlGet:")
			newLNClientTCP, newLNClientTCPErr := net.DialTCP("tcp", d.lnLocalTCPAddr, newHSAddr)
			if newLNClientTCPErr != nil {
				checkWarning(newLNClientTCPErr, "error starting TCP connection from kvslib to NEW HS upon receiving failed HS msg in handlGet: ")
				continue
			}
			newLNClientTCP.SetLinger(0)
			d.leaderClientRPC = rpc.NewClient(newLNClientTCP)

			util.PrintfYellow("%s\n", "Before leaderReconfiguredDone")
			d.leaderReconfiguredDone <- true
			util.PrintfYellow("%s\n", "After leaderReconfiguredDone")
			d.leaderNodeLock.Unlock()
		} else if serverFailure.ServerPosition == StopServer {
			d.allErrRecvd <- true
			return
		}
	}
}

func (d *KVS) sender() {
	// queue of operations
	for {
		req := <-d.reqQueue
		if req.kind == "PUT" {
			// Send to leader server
			d.putTrace = req.tracer.CreateTrace()
			d.putTrace.RecordAction(Put{req.pq.ClientId, req.pq.OpId, req.pq.Key, req.pq.Value})
			var putReq PutRequest = PutRequest{req.pq.ClientId, req.pq.OpId, req.pq.Key, req.pq.Value, d.putTrace.GenerateToken()}

			// var err error
			keepSending := true
			var putRes PutResponse
			for keepSending {
				d.leaderNodeLock.Lock()
				/*TO TEST: this is to deal with PUT blocking when previously using d.hsClientRPC.Call and having putRes := <-d.tpl.PutResChan outside of for-loop*/
				putDoneChan := d.leaderClientRPC.Go("Server.Put", putReq, &putRes, nil)
				// TODO: this is just to stop keepSending from running into an infinite loop for Milestone 2
				//       We likely need to add something to the commented out select block below once leader reconfiguration is added
				// if putDoneChan.Error != nil { // will now wait for head server to have finished reconfiguring
				// 	<-d.leaderReconfiguredDone
				// } else {
				// 	// <-putDoneChan.Done
				// 	// keepSending = false
				// 	select {
				// 		case <-putDoneChan.Done:
				// 			keepSending = false
				// 		case <-d.leaderReconfiguredDone:
				// 		// Send again
				// 		// case <-time.After(2 * time.Second): // set a timeout of 2 second
				// 			// Send again
				// 	}
				// }
				select {
				case <-putDoneChan.Done:
					if putDoneChan.Error != nil {
						// resend request if remote received an error
						util.PrintfPurple("%s%s\n", "Error received by kvslib, from Leader: ", putDoneChan.Error.Error())
						<-time.After(2 * time.Second)
					} else {
						keepSending = false
					}
				case <-d.leaderReconfiguredDone:
					// Send again
					// case <-time.After(2 * time.Second): // set a timeout of 2 second
					// Send again
				}

				//keepSending = false
				d.leaderNodeLock.Unlock()

			}

			// waits until the tail server gets back with put success
			// putRes := <-d.tpl.PutResChan  /* This is commented out because this will block d.hsClientRPC.Call from finishing*/
			if _, ok := d.resRecvd[req.pq.OpId]; !ok && putRes.Key != "Duplicate" { // don't do anything if receiving a duplicate response
				d.putTrace = req.tracer.ReceiveToken(putRes.Token)
				d.putTrace.RecordAction(PutResultRecvd{putRes.OpId, putRes.Key})
				d.resRecvd[req.pq.OpId] = putRes
				d.notifyCh <- ResultStruct{putRes.OpId, putRes.Value}
			}
		} else if req.kind == "GET" {
			// Send to tail server

			d.getTrace = req.tracer.CreateTrace()
			d.getTrace.RecordAction(Get{req.gq.ClientId, req.gq.OpId, req.gq.Key})

			var getReq GetRequest = GetRequest{req.gq.ClientId, req.gq.OpId, req.gq.Key, d.getTrace.GenerateToken()}
			var getRes GetResponse

			//var err error
			keepSending := true
			util.PrintfYellow("%s\n", "In Get, before keepSending loop")
			for keepSending {
				util.PrintfYellow("%s\n", "In Get, inside keepSending loop")
				//d.tailServerLock.Lock()
				d.leaderNodeLock.Lock()
				util.PrintfYellow("%s\n", "In Get, after Lock, before call")
				getDoneChan := d.leaderClientRPC.Go("Server.Get", getReq, &getRes, nil)
				select {
				case <-getDoneChan.Done:
					if getDoneChan.Error != nil {
						// resend request if remote received an error
						util.PrintfPurple("%s%s\n", "Error received by kvslib, from Leader: ", getDoneChan.Error.Error())
						<-time.After(2 * time.Second)
					} else {
						keepSending = false
						d.getTrace = req.tracer.ReceiveToken(getRes.Token)
						d.getTrace.RecordAction(GetResultRecvd{getRes.OpId, getRes.Key, getRes.Value})
					}
				case <-d.leaderReconfiguredDone:
					// Send again
					// case <-time.After(2 * time.Second): // set a timeout of 2 second
					// Send again
				}
				util.PrintfYellow("%s\n", "In Get, after Lock, after call")
				d.leaderNodeLock.Unlock()
				util.PrintfYellow("%s\n", "In Get, after unlock")
				//keepSending = false
				// d.leaderNodeLock.Unlock()
				// if putDoneChan.Error != nil { // will now wait for head server to have finished reconfiguring
				// 	<-d.leaderReconfiguredDone
				// } else {
				// 	<-putDoneChan.Done
				// 	keepSending = false
				// 	d.getTrace = req.tracer.ReceiveToken(getRes.Token)
				// 	d.getTrace.RecordAction(GetResultRecvd{getRes.OpId, getRes.Key, getRes.Value})
				// }

			}
		} else if req.kind == "STOP" {
			d.allResRecvd <- true
			return
		}
	}
}

// Stop Stops the KVS instance from communicating with the KVS and from delivering any results via the notify-channel.
// This call always succeeds.
func (d *KVS) Stop() {
	d.reqQueue <- Req{PutRequest{}, GetRequest{}, "STOP", nil}
	d.cnl.ServerFailChan <- ServerFail{ServerPosition: StopServer}
	<-d.allResRecvd // Wait until all pending request have received response from the server?
	// d.allResRecvd.Wait() // Wait until all pending request have received response from the server?
	// TODO: Confirm - Close the notify channel and all RPC connections before stop???
	<-d.allErrRecvd
	// close(d.notifyCh)
	// close(d.cnl.ServerFailChan)
	// close(d.tpl.PutResChan)
	// close(d.reqQueue)
	d.coordClientRPC.Close()
	d.leaderClientRPC.Close()
	// d.tsClientRPC.Close()
	// d.putTCPListener.Close()
	d.serverFailListener.Close()
	d.ktrace.GenerateToken()

	d.ktrace.RecordAction(KvslibStop{d.clientId})

	// TODO: Confirm - is this enough for closing?
}

func (tpl *ServerListener) PutSuccess(putRes PutResponse, isDone *bool) error {
	// util.PrintfCyan("%s\n", "in PutSuccess")
	tpl.PutResChan <- putRes
	// util.PrintfCyan("%s\n", "in PutSuccess")
	*isDone = true
	return nil
}

/*func (cnl *CoordListener) ChangeLeaderNode(newServerIPData NewLeaderStruct, isDone *bool) error {
	tracer := tracing.
	cnl.ServerFailChan <- ServerFail{Leader, newServerIPData.ip}
	*isDone = true
	util.PrintfGreen("\nReceived new leader: %v\n", newServerIPData.ip)
	return nil
}*/

func (d *KVS) ChangeLeaderNode(newServerIPData NewLeaderStruct, reply *NewLeaderChangedStruct) error {
	newTrace := d.kTracer.ReceiveToken(newServerIPData.Token)
	newTrace.RecordAction(LeaderNodeUpdateRecvd{
		ClientId: d.clientId,
		ServerId: newServerIPData.ServerNum,
	})

	d.cnl.ServerFailChan <- ServerFail{Leader, newServerIPData.Ip}
	*reply = NewLeaderChangedStruct{
		Token: newTrace.GenerateToken(),
	}
	util.PrintfGreen("\nReceived new leader: %v\n", newServerIPData.ServerNum)
	return nil
}

func checkCriticalErr(origErr error, errStartStr string) error {
	var newErr error = nil
	if origErr != nil {
		newErr = errors.New(errStartStr + origErr.Error())
		util.PrintfRed("%s%s\n", "SHOULD NOT HAPPEN: ", newErr.Error())
	}
	return newErr
}

func checkWarning(origErr error, errStartStr string) {
	if origErr != nil {
		util.PrintfYellow("%s%s\n", "WARNING: ", errStartStr+origErr.Error())
	}
}

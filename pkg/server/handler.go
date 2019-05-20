/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package server

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	admin "sofastack.io/sofa-mosn/pkg/admin/store"
	v2 "sofastack.io/sofa-mosn/pkg/api/v2"
	mosnctx "sofastack.io/sofa-mosn/pkg/context"
	"sofastack.io/sofa-mosn/pkg/filter/accept/originaldst"
	"sofastack.io/sofa-mosn/pkg/log"
	"sofastack.io/sofa-mosn/pkg/metrics"
	"sofastack.io/sofa-mosn/pkg/mtls"
	"sofastack.io/sofa-mosn/pkg/network"
	"sofastack.io/sofa-mosn/pkg/types"
	"sofastack.io/sofa-mosn/pkg/utils"
	"golang.org/x/sys/unix"
)

// ConnectionHandler
// ClusterConfigFactoryCb
// ClusterHostFactoryCb
type connHandler struct {
	numConnections int64
	listeners      []*activeListener
	clusterManager types.ClusterManager
}

// NewHandler
// create types.ConnectionHandler's implement connHandler
// with cluster manager and logger
func NewHandler(clusterManagerFilter types.ClusterManagerFilter, clMng types.ClusterManager) types.ConnectionHandler {
	ch := &connHandler{
		numConnections: 0,
		clusterManager: clMng,
		listeners:      make([]*activeListener, 0),
	}

	clusterManagerFilter.OnCreated(ch, ch)

	return ch
}

// ClusterConfigFactoryCb
func (ch *connHandler) UpdateClusterConfig(clusters []v2.Cluster) error {

	for _, cluster := range clusters {
		if !ch.clusterManager.AddOrUpdatePrimaryCluster(cluster) {
			return fmt.Errorf("UpdateClusterConfig: AddOrUpdatePrimaryCluster failure, cluster name = %s", cluster.Name)
		}
	}

	// TODO: remove cluster

	return nil
}

// ClusterHostFactoryCb
func (ch *connHandler) UpdateClusterHost(cluster string, priority uint32, hosts []v2.Host) error {
	return ch.clusterManager.UpdateClusterHosts(cluster, priority, hosts)
}

// ConnectionHandler
func (ch *connHandler) NumConnections() uint64 {
	return uint64(atomic.LoadInt64(&ch.numConnections))
}

// AddOrUpdateListener used to add or update listener
// listener name is unique key to represent the listener
// and listener with the same name must have the same configured address
func (ch *connHandler) AddOrUpdateListener(lc *v2.Listener, networkFiltersFactories []types.NetworkFilterChainFactory,
	streamFiltersFactories []types.StreamFilterChainFactory) (types.ListenerEventListener, error) {

	var listenerName string
	if lc.Name == "" {
		listenerName = utils.GenerateUUID()
		lc.Name = listenerName
	} else {
		listenerName = lc.Name
	}

	var al *activeListener
	if al = ch.findActiveListenerByName(listenerName); al != nil {
		// listener already exist, update the listener

		// a listener with the same name must have the same configured address
		if al.listener.Addr().String() != lc.Addr.String() ||
			al.listener.Addr().Network() != lc.Addr.Network() {
			return nil, errors.New("error updating listener, listen address and listen name doesn't match")
		}
		// currently, we just support one filter chain
		if len(lc.FilterChains) != 1 {
			return nil, errors.New("error updating listener, listener have filter chains count is not 1")
		}
		rawConfig := al.listener.Config()
		// FIXME: update log level need the pkg/logger support.

		// only chaned if not nil
		if networkFiltersFactories != nil {
			log.DefaultLogger.Infof("[server] [AddOrUpdateListener] [update] update network filters")
			al.networkFiltersFactories = networkFiltersFactories
			rawConfig.FilterChains[0].FilterChainMatch = lc.FilterChains[0].FilterChainMatch
			rawConfig.FilterChains[0].Filters = lc.FilterChains[0].Filters
		}
		if streamFiltersFactories != nil {
			log.DefaultLogger.Infof("[server] [AddOrUpdateListener] [update] update stream filters")
			al.streamFiltersFactoriesStore.Store(streamFiltersFactories)
			rawConfig.StreamFilters = lc.StreamFilters
		}

		// tls update only take effects on new connections
		// config changed
		rawConfig.FilterChains[0].TLSContexts = lc.FilterChains[0].TLSContexts
		rawConfig.FilterChains[0].TLSConfig = lc.FilterChains[0].TLSConfig
		rawConfig.FilterChains[0].TLSConfigs = lc.FilterChains[0].TLSConfigs
		rawConfig.Inspector = lc.Inspector
		mgr, err := mtls.NewTLSServerContextManager(rawConfig, al.listener, log.DefaultLogger)
		if err != nil {
			log.DefaultLogger.Errorf("[server] [conn handler] [update listener] create tls context manager failed, %v", err)
			return nil, err
		}
		// object changed
		al.tlsMng = mgr
		// some simle config update
		rawConfig.PerConnBufferLimitBytes = lc.PerConnBufferLimitBytes
		al.listener.SetPerConnBufferLimitBytes(lc.PerConnBufferLimitBytes)
		rawConfig.ListenerTag = lc.ListenerTag
		al.listener.SetListenerTag(lc.ListenerTag)
		rawConfig.HandOffRestoredDestinationConnections = lc.HandOffRestoredDestinationConnections
		al.listener.SetHandOffRestoredDestinationConnections(lc.HandOffRestoredDestinationConnections)

		al.listener.SetConfig(rawConfig)

		// set update label to true, do not start the listener again
		al.updatedLabel = true
		log.DefaultLogger.Infof("[server] [conn handler] [update listener] update listener: %s", lc.AddrConfig)

	} else {
		// listener doesn't exist, add the listener
		//TODO: connection level stop-chan usage confirm
		listenerStopChan := make(chan struct{})

		//initialize access log
		var als []types.AccessLog

		for _, alConfig := range lc.AccessLogs {

			//use default listener access log path
			if alConfig.Path == "" {
				alConfig.Path = types.MosnLogBasePath + string(os.PathSeparator) + lc.Name + "_access.log"
			}

			if al, err := log.NewAccessLog(alConfig.Path, nil, alConfig.Format); err == nil {
				als = append(als, al)
			} else {
				return nil, fmt.Errorf("initialize listener access logger %s failed: %v", alConfig.Path, err.Error())
			}
		}

		l := network.NewListener(lc)

		var err error
		al, err = newActiveListener(l, lc, als, networkFiltersFactories, streamFiltersFactories, ch, listenerStopChan)
		if err != nil {
			return al, err
		}
		l.SetListenerCallbacks(al)
		ch.listeners = append(ch.listeners, al)
		log.DefaultLogger.Infof("[server] [conn handler] [add listener] add listener: %s", lc.AddrConfig)
	}
	admin.SetListenerConfig(listenerName, *al.listener.Config())
	return al, nil
}

func (ch *connHandler) StartListener(lctx context.Context, listenerTag uint64) {
	for _, l := range ch.listeners {
		if l.listener.ListenerTag() == listenerTag {
			// TODO: use goroutine pool
			go l.listener.Start(lctx)
		}
	}
}

func (ch *connHandler) StartListeners(lctx context.Context) {
	for _, l := range ch.listeners {
		// start goroutine
		go l.listener.Start(lctx)
		// set listener addr metrics
		metrics.AddListenerAddr(l.listener.Addr().String())
	}
}

func (ch *connHandler) FindListenerByAddress(addr net.Addr) types.Listener {
	l := ch.findActiveListenerByAddress(addr)

	if l == nil {
		return nil
	}

	return l.listener
}

func (ch *connHandler) FindListenerByName(name string) types.Listener {
	l := ch.findActiveListenerByName(name)

	if l == nil {
		return nil
	}

	return l.listener
}

func (ch *connHandler) RemoveListeners(name string) {
	for i, l := range ch.listeners {
		if l.listener.Name() == name {
			log.DefaultLogger.Infof("[server] [conn handler] remove listener name: %s", name)
			ch.listeners = append(ch.listeners[:i], ch.listeners[i+1:]...)
		}
	}
}

func (ch *connHandler) StopListener(lctx context.Context, name string, close bool) error {
	for _, l := range ch.listeners {
		if l.listener.Name() == name {
			// stop goroutine
			if close {
				return l.listener.Close(lctx)
			}

			return l.listener.Stop()
		}
	}

	return nil
}

func (ch *connHandler) StopListeners(lctx context.Context, close bool) error {
	var errGlobal error
	for _, l := range ch.listeners {
		// stop goroutine
		if close {
			if err := l.listener.Close(lctx); err != nil {
				errGlobal = err
			}
		} else {
			if err := l.listener.Stop(); err != nil {
				errGlobal = err
			}
		}
	}

	return errGlobal
}

func (ch *connHandler) ListListenersFile(lctx context.Context) []*os.File {
	files := make([]*os.File, len(ch.listeners))

	for idx, l := range ch.listeners {
		file, err := l.listener.ListenerFile()
		if err != nil {
			log.DefaultLogger.Errorf("[server] [conn handler] fail to get listener %s file descriptor: %v", l.listener.Name(), err)
			return nil //stop reconfigure
		}
		files[idx] = file
	}
	return files
}

func (ch *connHandler) findActiveListenerByAddress(addr net.Addr) *activeListener {
	for _, l := range ch.listeners {
		if l.listener != nil {
			if l.listener.Addr().Network() == addr.Network() &&
				l.listener.Addr().String() == addr.String() {
				return l
			}
		}
	}

	return nil
}

func (ch *connHandler) findActiveListenerByName(name string) *activeListener {
	for _, l := range ch.listeners {
		if l.listener != nil {
			if l.listener.Name() == name {
				return l
			}
		}
	}

	return nil
}

func (ch *connHandler) StopConnection() {
	for _, l := range ch.listeners {
		close(l.stopChan)
	}
}

// ListenerEventListener
type activeListener struct {
	disableConnIo               bool
	listener                    types.Listener
	networkFiltersFactories     []types.NetworkFilterChainFactory
	streamFiltersFactoriesStore atomic.Value // store []types.StreamFilterChainFactory
	listenIP                    string
	listenPort                  int
	conns                       *list.List
	connsMux                    sync.RWMutex
	handler                     *connHandler
	stopChan                    chan struct{}
	stats                       *listenerStats
	accessLogs                  []types.AccessLog
	updatedLabel                bool
	tlsMng                      types.TLSContextManager
}

func newActiveListener(listener types.Listener, lc *v2.Listener, accessLoggers []types.AccessLog,
	networkFiltersFactories []types.NetworkFilterChainFactory, streamFiltersFactories []types.StreamFilterChainFactory,
	handler *connHandler, stopChan chan struct{}) (*activeListener, error) {
	al := &activeListener{
		disableConnIo:           lc.DisableConnIo,
		listener:                listener,
		networkFiltersFactories: networkFiltersFactories,
		conns:        list.New(),
		handler:      handler,
		stopChan:     stopChan,
		accessLogs:   accessLoggers,
		updatedLabel: false,
	}
	al.streamFiltersFactoriesStore.Store(streamFiltersFactories)

	listenPort := 0
	var listenIP string
	localAddr := al.listener.Addr().String()

	if temps := strings.Split(localAddr, ":"); len(temps) > 0 {
		listenPort, _ = strconv.Atoi(temps[len(temps)-1])
		listenIP = temps[0]
	}

	al.listenIP = listenIP
	al.listenPort = listenPort
	al.stats = newListenerStats(al.listener.Name())

	mgr, err := mtls.NewTLSServerContextManager(lc, listener, log.DefaultLogger)
	if err != nil {
		log.DefaultLogger.Errorf("[server] [new listener] create tls context manager failed, %v", err)
		return nil, err
	}
	al.tlsMng = mgr

	return al, nil
}

// ListenerEventListener
func (al *activeListener) OnAccept(rawc net.Conn, handOffRestoredDestinationConnections bool, oriRemoteAddr net.Addr, ch chan types.Connection, buf []byte) {
	var rawf *os.File

	// only store fd and tls conn handshake in final working listener
	if !handOffRestoredDestinationConnections {
		if !al.disableConnIo && network.UseNetpollMode {
			// store fd for further usage
			if tc, ok := rawc.(*net.TCPConn); ok {
				rawf, _ = tc.File()
			}
		}
		if al.tlsMng != nil && al.tlsMng.Enabled() {
			rawc = al.tlsMng.Conn(rawc)
		}
	}

	arc := newActiveRawConn(rawc, al)
	// TODO: create listener filter chain

	if handOffRestoredDestinationConnections {
		arc.acceptedFilters = append(arc.acceptedFilters, originaldst.NewOriginalDst())
		arc.handOffRestoredDestinationConnections = true
		log.DefaultLogger.Debugf("[server] [listener] accept restored destination connection from %s, remote addr:%s, origin remote addr:%s", al.listener.Addr().String(), rawc.RemoteAddr().String(), oriRemoteAddr.String())
	}

	ctx := mosnctx.WithValue(context.Background(), types.ContextKeyListenerPort, al.listenPort)
	ctx = mosnctx.WithValue(ctx, types.ContextKeyListenerType, al.listener.Config().Type)
	ctx = mosnctx.WithValue(ctx, types.ContextKeyListenerName, al.listener.Name())
	ctx = mosnctx.WithValue(ctx, types.ContextKeyNetworkFilterChainFactories, al.networkFiltersFactories)
	ctx = mosnctx.WithValue(ctx, types.ContextKeyStreamFilterChainFactories, &al.streamFiltersFactoriesStore)
	ctx = mosnctx.WithValue(ctx, types.ContextKeyAccessLogs, al.accessLogs)
	if rawf != nil {
		ctx = mosnctx.WithValue(ctx, types.ContextKeyConnectionFd, rawf)
	}
	if ch != nil {
		ctx = mosnctx.WithValue(ctx, types.ContextKeyAcceptChan, ch)
		ctx = mosnctx.WithValue(ctx, types.ContextKeyAcceptBuffer, buf)
	}
	if oriRemoteAddr != nil {
		ctx = mosnctx.WithValue(ctx, types.ContextOriRemoteAddr, oriRemoteAddr)
	}

	arc.ContinueFilterChain(ctx, true)
}

func (al *activeListener) OnNewConnection(ctx context.Context, conn types.Connection) {
	//Register Proxy's Filter
	filterManager := conn.FilterManager()
	for _, nfcf := range al.networkFiltersFactories {
		nfcf.CreateFilterChain(ctx, al.handler.clusterManager, filterManager)
	}
	filterManager.InitializeReadFilters()

	if len(filterManager.ListReadFilter()) == 0 &&
		len(filterManager.ListWriteFilters()) == 0 {
		// no filter found, close connection
		conn.Close(types.NoFlush, types.LocalClose)
		return
	}
	ac := newActiveConnection(al, conn)

	al.connsMux.Lock()
	e := al.conns.PushBack(ac)
	al.connsMux.Unlock()
	ac.element = e

	atomic.AddInt64(&al.handler.numConnections, 1)

	if log.DefaultLogger.GetLogLevel() >= log.DEBUG {
		log.DefaultLogger.Debugf("[server] [listener] accept connection from %s, condId= %d, remote addr:%s", al.listener.Addr().String(), conn.ID(), conn.RemoteAddr().String())
	}

	// todo: this hack is due to http2 protocol process. golang http2 provides a io loop to read/write stream
	if !al.disableConnIo {
		// start conn loops first
		conn.Start(ctx)
	}
}

func (al *activeListener) OnClose() {}

func (al *activeListener) removeConnection(ac *activeConnection) {
	al.connsMux.Lock()
	al.conns.Remove(ac.element)
	al.connsMux.Unlock()

	atomic.AddInt64(&al.handler.numConnections, -1)

}

func (al *activeListener) newConnection(ctx context.Context, rawc net.Conn) {
	conn := network.NewServerConnection(ctx, rawc, al.stopChan)
	oriRemoteAddr := mosnctx.Get(ctx, types.ContextOriRemoteAddr)
	if oriRemoteAddr != nil {
		conn.SetRemoteAddr(oriRemoteAddr.(net.Addr))
	}
	newCtx := mosnctx.WithValue(ctx, types.ContextKeyConnectionID, conn.ID())

	conn.SetBufferLimit(al.listener.PerConnBufferLimitBytes())

	al.OnNewConnection(newCtx, conn)
}

type activeRawConn struct {
	rawc                                  net.Conn
	rawf                                  *os.File
	originalDstIP                         string
	originalDstPort                       int
	oriRemoteAddr                         net.Addr
	handOffRestoredDestinationConnections bool
	rawcElement                           *list.Element
	activeListener                        *activeListener
	acceptedFilters                       []types.ListenerFilter
	acceptedFilterIndex                   int
}

func newActiveRawConn(rawc net.Conn, activeListener *activeListener) *activeRawConn {
	return &activeRawConn{
		rawc:           rawc,
		activeListener: activeListener,
	}
}

func (arc *activeRawConn) SetOriginalAddr(ip string, port int) {
	arc.originalDstIP = ip
	arc.originalDstPort = port
	arc.oriRemoteAddr, _ = net.ResolveTCPAddr("", ip+":"+strconv.Itoa(port))
	if log.DefaultLogger.GetLogLevel() >= log.INFO {
		log.DefaultLogger.Infof("[server] [conn] conn set origin addr:%s:%d", ip, port)
	}
}

func (arc *activeRawConn) HandOffRestoredDestinationConnectionsHandler(ctx context.Context) {
	var listener, localListener *activeListener

	for _, lst := range arc.activeListener.handler.listeners {
		if lst.listenIP == arc.originalDstIP && lst.listenPort == arc.originalDstPort {
			listener = lst
			break
		}

		if lst.listenPort == arc.originalDstPort && lst.listenIP == "0.0.0.0" {
			localListener = lst
		}
	}

	var ch chan types.Connection
	var buf []byte
	if val := mosnctx.Get(ctx, types.ContextKeyAcceptChan); val != nil {
		ch = val.(chan types.Connection)
		if val := mosnctx.Get(ctx, types.ContextKeyAcceptBuffer); val != nil {
			buf = val.([]byte)
		}
	}

	if listener != nil {
		if log.DefaultLogger.GetLogLevel() >= log.INFO {
			log.DefaultLogger.Infof("[server] [conn] original dst:%s:%d", listener.listenIP, listener.listenPort)
		}
		listener.OnAccept(arc.rawc, false, arc.oriRemoteAddr, ch, buf)
	}
	if localListener != nil {
		if log.DefaultLogger.GetLogLevel() >= log.INFO {
			log.DefaultLogger.Infof("[server] [conn] original dst:%s:%d", localListener.listenIP, localListener.listenPort)
		}
		localListener.OnAccept(arc.rawc, false, arc.oriRemoteAddr, ch, buf)
	}
}

func (arc *activeRawConn) ContinueFilterChain(ctx context.Context, success bool) {

	if !success {
		return
	}

	for ; arc.acceptedFilterIndex < len(arc.acceptedFilters); arc.acceptedFilterIndex++ {
		filterStatus := arc.acceptedFilters[arc.acceptedFilterIndex].OnAccept(arc)
		if filterStatus == types.Stop {
			return
		}
	}

	// TODO: handle hand_off_restored_destination_connections logic
	if arc.handOffRestoredDestinationConnections {
		arc.HandOffRestoredDestinationConnectionsHandler(ctx)
	} else {
		arc.activeListener.newConnection(ctx, arc.rawc)
	}

}

func (arc *activeRawConn) Conn() net.Conn {
	return arc.rawc
}

// ConnectionEventListener
// ListenerFilterManager note:unsupported now
// ListenerFilterCallbacks note:unsupported now
type activeConnection struct {
	element  *list.Element
	listener *activeListener
	conn     types.Connection
}

func newActiveConnection(listener *activeListener, conn types.Connection) *activeConnection {
	ac := &activeConnection{
		conn:     conn,
		listener: listener,
	}

	ac.conn.SetNoDelay(true)
	ac.conn.AddConnectionEventListener(ac)
	ac.conn.AddBytesReadListener(func(bytesRead uint64) {

		if bytesRead > 0 {
			listener.stats.DownstreamBytesReadTotal.Inc(int64(bytesRead))
		}
	})
	ac.conn.AddBytesSentListener(func(bytesSent uint64) {

		if bytesSent > 0 {
			listener.stats.DownstreamBytesWriteTotal.Inc(int64(bytesSent))
		}
	})

	return ac
}

// ConnectionEventListener
func (ac *activeConnection) OnEvent(event types.ConnectionEvent) {
	if event.IsClose() {
		ac.listener.removeConnection(ac)
	}
}

func sendInheritListeners() (net.Conn, error) {
	lf := ListListenersFile()
	if lf == nil {
		return nil, errors.New("ListListenersFile() error")
	}

	lsf, lerr := admin.ListServiceListenersFile()
	if lerr != nil {
		return nil, errors.New("ListServiceListenersFile() error")
	}

	var files []*os.File
	files = append(files, lf...)
	files = append(files, lsf...)

	if len(files) > 100 {
		log.DefaultLogger.Errorf("[server] InheritListener fd too many :%d", len(files))
		return nil, errors.New("InheritListeners too many")
	}
	fds := make([]int, len(files))
	for i, f := range files {
		fds[i] = int(f.Fd())
		log.DefaultLogger.Debugf("[server] InheritListener fd: %d", f.Fd())
		defer f.Close()
	}

	var unixConn net.Conn
	var err error
	// retry 10 time
	for i := 0; i < 10; i++ {
		unixConn, err = net.DialTimeout("unix", types.TransferListenDomainSocket, 1*time.Second)
		if err == nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		log.DefaultLogger.Errorf("[server] sendInheritListeners Dial unix failed %v", err)
		return nil, err
	}

	uc := unixConn.(*net.UnixConn)
	buf := make([]byte, 1)
	rights := syscall.UnixRights(fds...)
	n, oobn, err := uc.WriteMsgUnix(buf, rights, nil)
	if err != nil {
		log.DefaultLogger.Errorf("[server] WriteMsgUnix: %v", err)
		return nil, err
	}
	if n != len(buf) || oobn != len(rights) {
		log.DefaultLogger.Errorf("[server] WriteMsgUnix = %d, %d; want 1, %d", n, oobn, len(rights))
		return nil, err
	}

	return uc, nil
}

func GetInheritListeners() ([]net.Listener, net.Conn, error) {
	defer func() {
		if r := recover(); r != nil {
			log.StartLogger.Errorf("[server] getInheritListeners panic %v", r)
		}
	}()

	if !isReconfigure() {
		return nil, nil, nil
	}

	syscall.Unlink(types.TransferListenDomainSocket)

	l, err := net.Listen("unix", types.TransferListenDomainSocket)
	if err != nil {
		log.StartLogger.Errorf("[server] InheritListeners net listen error: %v", err)
		return nil, nil, err
	}
	defer l.Close()

	log.StartLogger.Infof("[server] Get InheritListeners start")

	ul := l.(*net.UnixListener)
	ul.SetDeadline(time.Now().Add(time.Second * 10))
	uc, err := ul.AcceptUnix()
	if err != nil {
		log.StartLogger.Errorf("[server] InheritListeners Accept error :%v", err)
		return nil, nil, err
	}
	log.StartLogger.Infof("[server] Get InheritListeners Accept")

	buf := make([]byte, 1)
	oob := make([]byte, 1024)
	_, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, nil, err
	}
	scms, err := unix.ParseSocketControlMessage(oob[0:oobn])
	if err != nil {
		log.StartLogger.Errorf("[server] ParseSocketControlMessage: %v", err)
		return nil, nil, err
	}
	if len(scms) != 1 {
		log.StartLogger.Errorf("[server] expected 1 SocketControlMessage; got scms = %#v", scms)
		return nil, nil, err
	}
	gotFds, err := unix.ParseUnixRights(&scms[0])
	if err != nil {
		log.StartLogger.Errorf("[server] unix.ParseUnixRights: %v", err)
		return nil, nil, err
	}

	listeners := make([]net.Listener, len(gotFds))
	for i := 0; i < len(gotFds); i++ {
		fd := uintptr(gotFds[i])
		file := os.NewFile(fd, "")
		if file == nil {
			log.StartLogger.Errorf("[server] create new file from fd %d failed", fd)
			return nil, nil, err
		}
		defer file.Close()

		fileListener, err := net.FileListener(file)
		if err != nil {
			log.StartLogger.Errorf("[server] recover listener from fd %d failed: %s", fd, err)
			return nil, nil, err
		}
		if listener, ok := fileListener.(*net.TCPListener); ok {
			listeners[i] = listener
		} else {
			log.StartLogger.Errorf("[server] listener recovered from fd %d is not a tcp listener", fd)
			return nil, nil, errors.New("not a tcp listener")
		}
	}

	return listeners, uc, nil
}

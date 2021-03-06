package memberlist

import (
	"net"
	"reflect"
	"sync/atomic"
	"time"
)

type nodeStateType int

const (
	stateAlive nodeStateType = iota
	stateSuspect
	stateDead
)

// Node represents a node in the cluster.
type Node struct {
	Name string
	Addr net.IP
	Meta []byte // Metadata from the delegate for this node.
}

// NodeState is used to manage our state view of another node
type nodeState struct {
	Node
	Incarnation uint32        // Last known incarnation number
	State       nodeStateType // Current state
	StateChange time.Time     // Time last state change happened
}

// ackHandler is used to register handlers for incoming acks
type ackHandler struct {
	handler func()
	timer   *time.Timer
}

// Schedule is used to ensure the Tick is performed periodically. This
// function is safe to call multiple times. If the memberlist is already
// scheduled, then it won't do anything.
func (m *Memberlist) schedule() {
	m.tickerLock.Lock()
	defer m.tickerLock.Unlock()

	// If we already have tickers, then don't do anything, since we're
	// scheduled
	if len(m.tickers) > 0 {
		return
	}

	// Create the stop tick channel, a blocking channel. We close this
	// when we should stop the tickers.
	stopCh := make(chan struct{})

	// Create a new probeTicker
	if m.config.ProbeInterval > 0 {
		t := time.NewTicker(m.config.ProbeInterval)
		go m.triggerFunc(t.C, stopCh, m.probe)
		m.tickers = append(m.tickers, t)
	}

	// Create a push pull ticker if needed
	if m.config.PushPullInterval > 0 {
		t := time.NewTicker(m.config.PushPullInterval)
		go m.triggerFunc(t.C, stopCh, m.pushPull)
		m.tickers = append(m.tickers, t)
	}

	// Create a gossip ticker if needed
	if m.config.GossipInterval > 0 && m.config.GossipNodes > 0 {
		t := time.NewTicker(m.config.GossipInterval)
		go m.triggerFunc(t.C, stopCh, m.gossip)
		m.tickers = append(m.tickers, t)
	}

	// If we made any tickers, then record the stopTick channel for
	// later.
	if len(m.tickers) > 0 {
		m.stopTick = stopCh
	}
}

// triggerFunc is used to trigger a function call each time a
// message is received until a stop tick arrives.
func (m *Memberlist) triggerFunc(C <-chan time.Time, stop <-chan struct{}, f func()) {
	for {
		select {
		case <-C:
			f()
		case <-stop:
			return
		}
	}
}

// Deschedule is used to stop the background maintenence. This is safe
// to call multiple times.
func (m *Memberlist) deschedule() {
	m.tickerLock.Lock()
	defer m.tickerLock.Unlock()

	// If we have no tickers, then we aren't scheduled.
	if len(m.tickers) == 0 {
		return
	}

	// Close the stop channel so all the ticker listeners stop.
	close(m.stopTick)

	// Explicitly stop all the tickers themselves so they don't take
	// up any more resources, and get rid of the list.
	for _, t := range m.tickers {
		t.Stop()
	}
	m.tickers = nil
}

// Tick is used to perform a single round of failure detection and gossip
func (m *Memberlist) probe() {
	// Track the number of indexes we've considered probing
	numCheck := 0
START:
	m.nodeLock.RLock()

	// Make sure we don't wrap around infinitely
	if numCheck >= len(m.nodes) {
		m.nodeLock.RUnlock()
		return
	}

	// Handle the wrap around case
	if m.probeIndex >= len(m.nodes) {
		m.nodeLock.RUnlock()
		m.resetNodes()
		m.probeIndex = 0
		numCheck++
		goto START
	}

	// Determine if we should probe this node
	skip := false
	var node nodeState

	node = *m.nodes[m.probeIndex]
	if node.Name == m.config.Name {
		skip = true
	} else if node.State == stateDead {
		skip = true
	}

	// Potentially skip
	m.nodeLock.RUnlock()
	m.probeIndex++
	if skip {
		numCheck++
		goto START
	}

	// Probe the specific node
	m.probeNode(&node)
}

// probeNode handles a single round of failure checking on a node
func (m *Memberlist) probeNode(node *nodeState) {
	// Send a ping to the node
	ping := ping{SeqNo: m.nextSeqNo()}
	destAddr := &net.UDPAddr{IP: node.Addr, Port: m.config.UDPPort}

	// Setup an ack handler
	ackCh := make(chan bool, m.config.IndirectChecks+1)
	m.setAckChannel(ping.SeqNo, ackCh, m.config.ProbeInterval)

	// Send the ping message
	if err := m.encodeAndSendMsg(destAddr, pingMsg, &ping); err != nil {
		m.logger.Printf("[ERR] Failed to send ping: %s", err)
		return
	}

	// Wait for response or round-trip-time
	select {
	case v := <-ackCh:
		if v == true {
			return
		}

		// As an edge case, if we get a timeout, we need to re-enqueue it
		// here to break out of the select below
		if v == false {
			ackCh <- v
		}
	case <-time.After(m.config.ProbeTimeout):
	}

	// Get some random live nodes
	m.nodeLock.RLock()
	excludes := []string{m.config.Name, node.Name}
	kNodes := kRandomNodes(m.config.IndirectChecks, excludes, m.nodes)
	m.nodeLock.RUnlock()

	// Attempt an indirect ping
	ind := indirectPingReq{SeqNo: ping.SeqNo, Target: node.Addr}
	for _, peer := range kNodes {
		destAddr := &net.UDPAddr{IP: peer.Addr, Port: m.config.UDPPort}
		if err := m.encodeAndSendMsg(destAddr, indirectPingMsg, &ind); err != nil {
			m.logger.Printf("[ERR] Failed to send indirect ping: %s", err)
		}
	}

	// Wait for the acks or timeout
	select {
	case v := <-ackCh:
		if v == true {
			return
		}
	}

	// No acks received from target, suspect
	s := suspect{Incarnation: node.Incarnation, Node: node.Name}
	m.suspectNode(&s)
}

// resetNodes is used when the tick wraps around. It will reap the
// dead nodes and shuffle the node list.
func (m *Memberlist) resetNodes() {
	m.nodeLock.Lock()
	defer m.nodeLock.Unlock()

	// Move the dead nodes
	deadIdx := moveDeadNodes(m.nodes)

	// Deregister the dead nodes
	for i := deadIdx; i < len(m.nodes); i++ {
		m.nodes[i] = nil
	}

	// Trim the nodes to exclude the dead nodes
	m.nodes = m.nodes[0:deadIdx]

	// Shuffle live nodes
	shuffleNodes(m.nodes)
}

// gossip is invoked every GossipInterval period to broadcast our gossip
// messages to a few random nodes.
func (m *Memberlist) gossip() {
	// Get some random live nodes
	m.nodeLock.RLock()
	excludes := []string{m.config.Name}
	kNodes := kRandomNodes(m.config.GossipNodes, excludes, m.nodes)
	m.nodeLock.RUnlock()

	// Compute the bytes available
	bytesAvail := udpSendBuf - compoundHeaderOverhead

	for _, node := range kNodes {
		// Get any pending broadcasts
		msgs := m.getBroadcasts(compoundOverhead, bytesAvail)
		if len(msgs) == 0 {
			return
		}

		// Create a compound message
		compound := makeCompoundMessage(msgs)

		// Send the compound message
		destAddr := &net.UDPAddr{IP: node.Addr, Port: m.config.UDPPort}
		if err := m.rawSendMsg(destAddr, compound.Bytes()); err != nil {
			m.logger.Printf("[ERR] Failed to send gossip to %s: %s", destAddr, err)
		}
	}
}

// pushPull is invoked periodically to randomly perform a complete state
// exchange. Used to ensure a high level of convergence, but is also
// reasonably expensive as the entire state of this node is exchanged
// with the other node.
func (m *Memberlist) pushPull() {
	// Get a random live node
	m.nodeLock.RLock()
	excludes := []string{m.config.Name}
	nodes := kRandomNodes(1, excludes, m.nodes)
	m.nodeLock.RUnlock()

	// If no nodes, bail
	if len(nodes) == 0 {
		return
	}
	node := nodes[0]

	// Attempt a push pull
	if err := m.pushPullNode(node.Addr); err != nil {
		m.logger.Printf("[ERR] Push/Pull with %s failed: %s", node.Name, err)
	}
}

// pushPullNode does a complete state exchange with a specific node.
func (m *Memberlist) pushPullNode(addr []byte) error {
	// Attempt to send and receive with the node
	remote, userState, err := m.sendAndReceiveState(addr)
	if err != nil {
		return err
	}

	// Merge the state
	m.mergeState(remote)

	// Invoke the delegate
	if m.config.Delegate != nil {
		m.config.Delegate.MergeRemoteState(userState)
	}
	return nil
}

// nextSeqNo returns a usable sequence number in a thread safe way
func (m *Memberlist) nextSeqNo() uint32 {
	return atomic.AddUint32(&m.sequenceNum, 1)
}

// nextIncarnation returns the next incarnation number in a thread safe way
func (m *Memberlist) nextIncarnation() uint32 {
	return atomic.AddUint32(&m.incarnation, 1)
}

// setAckChannel is used to attach a channel to receive a message when
// an ack with a given sequence number is received. The channel gets sent
// false on timeout
func (m *Memberlist) setAckChannel(seqNo uint32, ch chan bool, timeout time.Duration) {
	// Create a handler function
	handler := func() {
		select {
		case ch <- true:
		default:
		}
	}

	// Add the handler
	ah := &ackHandler{handler, nil}
	m.ackLock.Lock()
	m.ackHandlers[seqNo] = ah
	m.ackLock.Unlock()

	// Setup a reaping routing
	ah.timer = time.AfterFunc(timeout, func() {
		m.ackLock.Lock()
		delete(m.ackHandlers, seqNo)
		m.ackLock.Unlock()
		select {
		case ch <- false:
		default:
		}
	})
}

// setAckHandler is used to attach a handler to be invoked when an
// ack with a given sequence number is received. If a timeout is reached,
// the handler is deleted
func (m *Memberlist) setAckHandler(seqNo uint32, handler func(), timeout time.Duration) {
	// Add the handler
	ah := &ackHandler{handler, nil}
	m.ackLock.Lock()
	m.ackHandlers[seqNo] = ah
	m.ackLock.Unlock()

	// Setup a reaping routing
	ah.timer = time.AfterFunc(timeout, func() {
		m.ackLock.Lock()
		delete(m.ackHandlers, seqNo)
		m.ackLock.Unlock()
	})
}

// Invokes an Ack handler if any is associated, and reaps the handler immediately
func (m *Memberlist) invokeAckHandler(seqNo uint32) {
	m.ackLock.Lock()
	ah, ok := m.ackHandlers[seqNo]
	delete(m.ackHandlers, seqNo)
	m.ackLock.Unlock()
	if !ok {
		return
	}
	ah.timer.Stop()
	ah.handler()
}

// aliveNode is invoked by the network layer when we get a message about a
// live node.
func (m *Memberlist) aliveNode(a *alive) {
	m.nodeLock.Lock()
	defer m.nodeLock.Unlock()
	state, ok := m.nodeMap[a.Node]

	// It is possible that during a Leave(), there is already an aliveMsg
	// in-queue to be processed but blocked by the locks above. If we let
	// that aliveMsg process, it'll cause us to re-join the cluster. This
	// ensures that we don't.
	if m.leave && a.Node == m.config.Name {
		return
	}

	// Check if we've never seen this node before, and if not, then
	// store this node in our node map.
	if !ok {
		state = &nodeState{
			Node: Node{
				Name: a.Node,
				Addr: a.Addr,
				Meta: a.Meta,
			},
			State: stateDead,
		}

		// Add to map
		m.nodeMap[a.Node] = state

		// Get a random offset. This is important to ensure
		// the failure detection bound is low on average. If all
		// nodes did an append, failure detection bound would be
		// very high.
		n := len(m.nodes)
		offset := randomOffset(n)

		// Add at the end and swap with the node at the offset
		m.nodes = append(m.nodes, state)
		m.nodes[offset], m.nodes[n] = m.nodes[n], m.nodes[offset]
	}

	// Check if this address is different than the existing node
	if !reflect.DeepEqual([]byte(state.Addr), a.Addr) {
		m.logger.Printf("[WARN] Conflicting address for %s. Mine: %v Theirs: %v",
			state.Name, state.Addr, net.IP(a.Addr))
		return
	}

	// Bail if the incarnation number is old
	if a.Incarnation <= state.Incarnation {
		return
	}

	// Re-Broadcast
	m.encodeAndBroadcast(a.Node, aliveMsg, a)

	// Update the state and incarnation number
	oldState := state.State
	state.Incarnation = a.Incarnation
	if state.State != stateAlive {
		state.State = stateAlive
		state.StateChange = time.Now()
	}

	// if Dead -> Alive, notify of join
	if oldState == stateDead {
		if m.config.Events != nil {
			m.config.Events.NotifyJoin(&state.Node)
		}
	}
}

// suspectNode is invoked by the network layer when we get a message
// about a suspect node
func (m *Memberlist) suspectNode(s *suspect) {
	m.nodeLock.Lock()
	defer m.nodeLock.Unlock()
	state, ok := m.nodeMap[s.Node]

	// If we've never heard about this node before, ignore it
	if !ok {
		return
	}

	// Ignore old incarnation numbers
	if s.Incarnation < state.Incarnation {
		return
	}

	// Ignore non-alive nodes
	if state.State != stateAlive {
		return
	}

	// If this is us we need to refute, otherwise re-broadcast
	if state.Name == m.config.Name {
		inc := m.nextIncarnation()
		for s.Incarnation >= inc {
			inc = m.nextIncarnation()
		}
		state.Incarnation = inc

		a := alive{
			Incarnation: inc,
			Node:        state.Name,
			Addr:        state.Addr,
			Meta:        state.Meta,
		}
		m.encodeAndBroadcast(s.Node, aliveMsg, a)

		return // Do not mark ourself suspect
	} else {
		m.encodeAndBroadcast(s.Node, suspectMsg, s)
	}

	// Update the state
	state.Incarnation = s.Incarnation
	state.State = stateSuspect
	changeTime := time.Now()
	state.StateChange = changeTime

	// Setup a timeout for this
	timeout := suspicionTimeout(m.config.SuspicionMult, len(m.nodes), m.config.ProbeInterval)
	time.AfterFunc(timeout, func() {
		m.nodeLock.Lock()
		state, ok := m.nodeMap[s.Node]
		timeout := ok && state.State == stateSuspect && state.StateChange == changeTime
		m.nodeLock.Unlock()

		if timeout {
			m.suspectTimeout(state)
		}
	})
}

// suspectTimeout is invoked when a suspect timeout has occurred
func (m *Memberlist) suspectTimeout(n *nodeState) {
	// Construct a dead message
	d := dead{Incarnation: n.Incarnation, Node: n.Name}
	m.deadNode(&d)
}

// deadNode is invoked by the network layer when we get a message
// about a dead node
func (m *Memberlist) deadNode(d *dead) {
	m.nodeLock.Lock()
	defer m.nodeLock.Unlock()
	state, ok := m.nodeMap[d.Node]

	// If we've never heard about this node before, ignore it
	if !ok {
		return
	}

	// Ignore old incarnation numbers
	if d.Incarnation < state.Incarnation {
		return
	}

	// Ignore if node is already dead
	if state.State == stateDead {
		return
	}

	// Check if this is us
	if state.Name == m.config.Name {
		// If we are not leaving we need to refute
		if !m.leave {
			inc := m.nextIncarnation()
			for d.Incarnation >= inc {
				inc = m.nextIncarnation()
			}

			a := alive{Incarnation: inc, Node: state.Name, Addr: state.Addr, Meta: state.Meta}
			m.encodeAndBroadcast(d.Node, aliveMsg, a)

			state.Incarnation = inc
			return // Do not mark ourself dead
		}

		// If we are leaving, we broadcast and wait
		m.encodeBroadcastNotify(d.Node, deadMsg, d, m.leaveBroadcast)
	} else {
		m.encodeAndBroadcast(d.Node, deadMsg, d)
	}

	// Update the state
	state.Incarnation = d.Incarnation
	state.State = stateDead
	state.StateChange = time.Now()

	// Remove from the node map
	delete(m.nodeMap, state.Name)

	// Notify of death
	if m.config.Events != nil {
		m.config.Events.NotifyLeave(&state.Node)
	}
}

// mergeState is invoked by the network layer when we get a Push/Pull
// state transfer
func (m *Memberlist) mergeState(remote []pushNodeState) {
	for _, r := range remote {
		// Look for a matching local node
		m.nodeLock.RLock()
		local, ok := m.nodeMap[r.Name]
		sameState := ok && local.State == r.State && r.Name != m.config.Name
		m.nodeLock.RUnlock()

		// Skip if we agree on states
		if sameState {
			continue
		}

		switch r.State {
		case stateAlive:
			a := alive{
				Incarnation: r.Incarnation,
				Node:        r.Name,
				Addr:        r.Addr,
				Meta:        r.Meta,
			}
			m.aliveNode(&a)

		case stateDead:
			// If the remote node belives a node is dead, we prefer to
			// suspect that node instead of declaring it dead instantly
			fallthrough
		case stateSuspect:
			s := suspect{Incarnation: r.Incarnation, Node: r.Name}
			m.suspectNode(&s)
		}
	}
}

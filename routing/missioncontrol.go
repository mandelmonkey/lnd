package routing

import (
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
)

const (
	// vertexDecay is the decay period of colored vertexes added to
	// missionControl. Once vertexDecay passes after an entry has been
	// added to the prune view, it is garbage collected. This value is
	// larger than edgeDecay as an edge failure typical indicates an
	// unbalanced channel, while a vertex failure indicates a node is not
	// online and active.
	vertexDecay = time.Duration(time.Minute * 5)

	// edgeDecay is the decay period of colored edges added to
	// missionControl. Once edgeDecay passed after an entry has been added,
	// it is garbage collected. This value is smaller than vertexDecay as
	// an edge related failure during payment sending typically indicates
	// that a channel was unbalanced, a condition which may quickly change.
	//
	// TODO(roasbeef): instead use random delay on each?
	edgeDecay = time.Duration(time.Second * 5)
)

// missionControl contains state which summarizes the past attempts of HTLC
// routing by external callers when sending payments throughout the network.
// missionControl remembers the outcome of these past routing attempts (success
// and failure), and is able to provide hints/guidance to future HTLC routing
// attempts.  missionControl maintains a decaying network view of the
// edges/vertexes that should be marked as "pruned" during path finding. This
// graph view acts as a shared memory during HTLC payment routing attempts.
// With each execution, if an error is encountered, based on the type of error
// and the location of the error within the route, an edge or vertex is added
// to the view. Later sending attempts will then query the view for all the
// vertexes/edges that should be ignored. Items in the view decay after a set
// period of time, allowing the view to be dynamic w.r.t network changes.
type missionControl struct {
	// failedEdges maps a short channel ID to be pruned, to the time that
	// it was added to the prune view. Edges are added to this map if a
	// caller reports to missionControl a failure localized to that edge
	// when sending a payment.
	failedEdges map[uint64]time.Time

	// failedVertexes maps a node's public key that should be pruned, to
	// the time that it was added to the prune view. Vertexes are added to
	// this map if a caller reports to missionControl a failure localized
	// to that particular vertex.
	failedVertexes map[vertex]time.Time

	graph *channeldb.ChannelGraph

	selfNode *channeldb.LightningNode

	sync.Mutex

	// TODO(roasbeef): further counters, if vertex continually unavailable,
	// add to another generation

	// TODO(roasbeef): also add favorable metrics for nodes
}

// newMissionControl returns a new instance of missionControl.
//
// TODO(roasbeef): persist memory
func newMissionControl(g *channeldb.ChannelGraph,
	s *channeldb.LightningNode) *missionControl {

	return &missionControl{
		graph:          g,
		selfNode:       s,
		failedEdges:    make(map[uint64]time.Time),
		failedVertexes: make(map[vertex]time.Time),
	}
}

// ReportVertexFailure adds a vertex to the graph prune view after a client
// reports a routing failure localized to the vertex. The time the vertex was
// added is noted, as it'll be pruned from the view after a period of
// vertexDecay.
func (m *missionControl) ReportVertexFailure(v vertex) {
	log.Debugf("Reporting vertex %v failure to Mission Control", v)

	m.Lock()
	m.failedVertexes[v] = time.Now()
	m.Unlock()
}

// ReportChannelFailure adds a channel to the graph prune view. The time the
// channel was added is noted, as it'll be pruned from the view after a period
// of edgeDecay.
//
// TODO(roasbeef): also add value attempted to send and capacity of channel
func (m *missionControl) ReportChannelFailure(e uint64) {
	log.Debugf("Reporting edge %v failure to Mission Control", e)

	m.Lock()
	m.failedEdges[e] = time.Now()
	m.Unlock()
}

// RequestRoute returns a route which is likely to be capable for successfully
// routing the specified HTLC payment to the target node. Initially the first
// set of paths returned from this method may encounter routing failure along
// the way, however as more payments are sent, mission control will start to
// build an up to date view of the network itself. With each payment a new area
// will be explored, which feeds into the recommendations made for routing.
//
// NOTE: This function is safe for concurrent access.
func (m *missionControl) RequestRoute(payment *LightningPayment,
	height uint32) (*Route, error) {

	// First, we'll query mission control for it's current recommendation
	// on the edges/vertexes to ignore during path finding.
	pruneView := m.GraphPruneView()

	// TODO(roasbeef): sync logic amongst dist sys

	// Taking into account this prune view, we'll attempt to locate a path
	// to our destination, respecting the recommendations from
	// missionControl.
	path, err := findPath(nil, m.graph, m.selfNode, payment.Target,
		pruneView.vertexes, pruneView.edges, payment.Amount)
	if err != nil {
		return nil, err
	}

	// With the next candidate path found, we'll attempt to turn this into
	// a route by applying the time-lock and fee requirements.
	sourceVertex := newVertex(m.selfNode.PubKey)
	route, err := newRoute(payment.Amount, sourceVertex, path, height)
	if err != nil {
		// TODO(roasbeef): return which edge/vertex didn't work
		// out
		return nil, err
	}

	return route, err
}

// GraphPruneView returns a new graphPruneView instance which is to be
// consulted during path finding. If a vertex/edge is found within the returned
// prune view, it is to be ignored as a goroutine has had issues routing
// through it successfully. Within this method the main view of the
// missionControl is garbage collected as entires are detected to be "stale".
func (m *missionControl) GraphPruneView() *graphPruneView {
	// First, we'll grab the current time, this value will be used to
	// determine if an entry is stale or not.
	now := time.Now()

	m.Lock()

	// For each of the vertexes that have been added to the prune view, if
	// it is now "stale", then we'll ignore it and avoid adding it to the
	// view we'll return.
	vertexes := make(map[vertex]struct{})
	for vertex, pruneTime := range m.failedVertexes {
		if now.Sub(pruneTime) >= vertexDecay {
			log.Tracef("Pruning decayed failure report for vertex %v "+
				"from Mission Control", vertex)

			delete(m.failedVertexes, vertex)
			continue
		}

		vertexes[vertex] = struct{}{}
	}

	// We'll also do the same for edges, but use the edgeDecay this time
	// rather than the decay for vertexes.
	edges := make(map[uint64]struct{})
	for edge, pruneTime := range m.failedEdges {
		if now.Sub(pruneTime) >= edgeDecay {
			log.Tracef("Pruning decayed failure report for edge %v "+
				"from Mission Control", edge)

			delete(m.failedEdges, edge)
			continue
		}

		edges[edge] = struct{}{}
	}

	m.Unlock()

	log.Debugf("Mission Control returning prune view of %v edges, %v "+
		"vertexes", len(edges), len(vertexes))

	return &graphPruneView{
		edges:    edges,
		vertexes: vertexes,
	}
}

// graphPruneView is a filter of sorts that path finding routines should
// consult during the execution. Any edges or vertexes within the view should
// be ignored during path finding. The contents of the view reflect the current
// state of the wider network from the PoV of mission control compiled via HTLC
// routing attempts in the past.
type graphPruneView struct {
	edges map[uint64]struct{}

	vertexes map[vertex]struct{}
}

// ResetHistory resets the history of missionControl returning it to a state as
// if no payment attempts have been made.
func (m *missionControl) ResetHistory() {
	m.Lock()
	m.failedEdges = make(map[uint64]time.Time)
	m.failedVertexes = make(map[vertex]time.Time)
	m.Unlock()
}

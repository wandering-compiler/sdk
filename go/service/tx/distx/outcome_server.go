package distx

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	distxpb "github.com/wandering-compiler/sdk/go/pb/common/distx"
	"github.com/wandering-compiler/sdk/go/service/tx/txregistry"
)

// OutcomeServer implements [distxpb.W17TransactionOutcomeServer] over the
// same [*txregistry.Memory] the coordinator's [Server] drives: it tells a
// process that took part in a transaction without running it — a business
// method reached over the wire with the caller's `w17-tx-id` — how that
// transaction ended, so the method's events go out on the commit and never on
// a rollback ([RemoteCommits] is the client half).
//
// A separate service from W17DistributedTransaction so adding it changed no
// existing client interface (hand-written fakes of that client keep
// compiling). A storage bundle registers both on the same registry.
type OutcomeServer struct {
	distxpb.UnimplementedW17TransactionOutcomeServer
	registry *txregistry.Memory
}

// NewOutcomeServer wraps reg. Panics on nil, like [NewServer].
func NewOutcomeServer(reg *txregistry.Memory) *OutcomeServer {
	if reg == nil {
		panic("distx.NewOutcomeServer: registry is required")
	}
	return &OutcomeServer{registry: reg}
}

// Await streams PENDING once the waiter is registered (while the transaction
// is open), then COMMITTED or NOT_COMMITTED when it ends, and returns. An id
// that already ended gets only the last message; one the registry does not
// know answers NotFound, with the ended detail when it remembers.
func (s *OutcomeServer) Await(req *distxpb.AwaitRequest, stream grpc.ServerStreamingServer[distxpb.AwaitResponse]) error {
	txID := req.GetTxId()
	ch, cancel, ok := s.registry.AwaitEnd(txID)
	if !ok {
		return status.Errorf(codes.NotFound, "await: %v %q", txregistry.ErrUnknownTxID, txID)
	}
	defer cancel()
	select {
	case ended := <-ch:
		return sendOutcome(stream, ended)
	default:
	}
	if err := stream.Send(&distxpb.AwaitResponse{Outcome: distxpb.TxOutcome_TX_OUTCOME_PENDING}); err != nil {
		return err
	}
	select {
	case ended := <-ch:
		return sendOutcome(stream, ended)
	case <-stream.Context().Done():
		return status.FromContextError(stream.Context().Err()).Err()
	}
}

// sendOutcome sends the last message for a settled registry kind. AwaitEnd
// delivers only settled kinds; anything else is a bug, answered as one rather
// than as an outcome a caller would act on.
func sendOutcome(stream grpc.ServerStreamingServer[distxpb.AwaitResponse], e txregistry.Ended) error {
	var out distxpb.TxOutcome
	switch e {
	case txregistry.EndedCommitted:
		out = distxpb.TxOutcome_TX_OUTCOME_COMMITTED
	case txregistry.EndedNotCommitted:
		out = distxpb.TxOutcome_TX_OUTCOME_NOT_COMMITTED
	default:
		return status.Errorf(codes.Internal, "await: the registry reported an unsettled outcome %v", e)
	}
	return stream.Send(&distxpb.AwaitResponse{Outcome: out})
}

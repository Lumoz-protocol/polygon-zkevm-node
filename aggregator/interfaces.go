package aggregator

import (
	"context"
	"math/big"

	"github.com/0xPolygonHermez/zkevm-node/aggregator/pb"
	ethmanTypes "github.com/0xPolygonHermez/zkevm-node/etherman/types"
	"github.com/0xPolygonHermez/zkevm-node/ethtxmanager"
	"github.com/0xPolygonHermez/zkevm-node/state"
	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v4"
)

// Consumer interfaces required by the package.

type proverInterface interface {
	Name() string
	ID() string
	Addr() string
	IsIdle() (bool, error)
	BatchProof(input *pb.InputProver) (*string, error)
	AggregatedProof(inputProof1, inputProof2 string) (*string, error)
	FinalProof(inputProof string, aggregatorAddr string) (*string, error)
	WaitRecursiveProof(ctx context.Context, proofID string) (string, error)
	WaitFinalProof(ctx context.Context, proofID string) (*pb.FinalProof, error)
}

// ethTxManager contains the methods required to send txs to
// ethereum.
type ethTxManager interface {
	Add(ctx context.Context, owner, id string, from common.Address, to *common.Address, value *big.Int, data []byte, dbTx pgx.Tx) error
	Result(ctx context.Context, owner, id string, dbTx pgx.Tx) (ethtxmanager.MonitoredTxResult, error)
	ResultsByStatus(ctx context.Context, owner string, statuses []ethtxmanager.MonitoredTxStatus, dbTx pgx.Tx) ([]ethtxmanager.MonitoredTxResult, error)
	ProcessPendingMonitoredTxs(ctx context.Context, owner string, failedResultHandler ethtxmanager.ResultHandler, dbTx pgx.Tx)
	AddReSendTx(ctx context.Context, id string, dbTx pgx.Tx) (bool, error)
	UpdateId(ctx context.Context, id string, dbTx pgx.Tx) error
}

// etherman contains the methods required to interact with ethereum
type etherman interface {
	GetLatestVerifiedBatchNum() (uint64, error)
	BuildTrustedVerifyBatchesTxData(lastVerifiedBatch, newVerifiedBatch uint64, inputs *ethmanTypes.FinalProofInputs) (to *common.Address, data []byte, err error)
	BuildProofHashTxData(lastVerifiedBatch, newVerifiedBatch uint64, proofHash common.Hash) (to *common.Address, data []byte, err error)
	BuildUnTrustedVerifyBatchesTxData(lastVerifiedBatch, newVerifiedBatch uint64, inputs *ethmanTypes.FinalProofInputs) (to *common.Address, data []byte, err error)
	TrustedAggregator() (common.Address, error)
	GetLatestBlockNumber(ctx context.Context) (uint64, error)
	JudgeAggregatorDeposit(account common.Address) (bool, error)
	GetSequencedBatch(finalBatchNum uint64) (uint64, error)
}

// aggregatorTxProfitabilityChecker interface for different profitability
// checking algorithms.
type aggregatorTxProfitabilityChecker interface {
	IsProfitable(context.Context, *big.Int) (bool, error)
}

// stateInterface gathers the methods to interact with the state.
type stateInterface interface {
	BeginStateTransaction(ctx context.Context) (pgx.Tx, error)
	CheckProofContainsCompleteSequences(ctx context.Context, proof *state.Proof, dbTx pgx.Tx) (bool, error)
	GetLastVerifiedBatch(ctx context.Context, dbTx pgx.Tx) (*state.VerifiedBatch, error)
	GetProofReadyToVerify(ctx context.Context, lastVerfiedBatchNumber uint64, dbTx pgx.Tx) (*state.Proof, error)
	GetVirtualBatchToProve(ctx context.Context, lastVerfiedBatchNumber uint64, dbTx pgx.Tx) (*state.Batch, error)
	GetProofsToAggregate(ctx context.Context, dbTx pgx.Tx) (*state.Proof, *state.Proof, error)
	GetBatchByNumber(ctx context.Context, batchNumber uint64, dbTx pgx.Tx) (*state.Batch, error)
	AddGeneratedProof(ctx context.Context, proof *state.Proof, dbTx pgx.Tx) error
	UpdateGeneratedProof(ctx context.Context, proof *state.Proof, dbTx pgx.Tx) error
	DeleteGeneratedProofs(ctx context.Context, batchNumber uint64, batchNumberFinal uint64, dbTx pgx.Tx) error
	DeleteUngeneratedProofs(ctx context.Context, dbTx pgx.Tx) error
	CleanupGeneratedProofs(ctx context.Context, batchNumber uint64, dbTx pgx.Tx) error
	CleanupLockedProofs(ctx context.Context, duration string, dbTx pgx.Tx) (int64, error)
	GetEarlyProofHashByNumber(ctx context.Context, batchNumber uint64, dbTx pgx.Tx) (uint64, error)
	GetLastBlock(ctx context.Context, dbTx pgx.Tx) (*state.Block, error)
	GetProofHashBySender(ctx context.Context, sender string, batchNumber, minCommit, lastBlockNumber uint64, dbTx pgx.Tx) (string, error)
	GetProverProofByHash(ctx context.Context, hash string, batchNumberFinal uint64, dbTx pgx.Tx) (*state.ProverProof, error)
	AddProverProof(ctx context.Context, proverProof *state.ProverProof, dbTx pgx.Tx) error
	AddFinalProof(ctx context.Context, finalProof *state.FinalProof, dbTx pgx.Tx) error
	GetFinalProofByMonitoredId(ctx context.Context, monitoredId string, dbTx pgx.Tx) (*state.FinalProof, error)
	GetSequence(ctx context.Context, lastVerifiedBatchNumber uint64, dbTx pgx.Tx) (state.Sequence, error)
	GetStatusDoneBlockNum(ctx context.Context, id string, dbTx pgx.Tx) (uint64, error)
	HaveProverProofByBatchNum(ctx context.Context, batchNumberFinal uint64, dbTx pgx.Tx) (bool, error)
}

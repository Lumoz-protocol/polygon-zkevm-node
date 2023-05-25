package state

import (
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Batch struct
type Batch struct {
	BatchNumber    uint64
	Coinbase       common.Address
	BatchL2Data    []byte
	StateRoot      common.Hash
	LocalExitRoot  common.Hash
	AccInputHash   common.Hash
	Timestamp      time.Time
	Transactions   []types.Transaction
	GlobalExitRoot common.Hash
	ForcedBatchNum *uint64
}

// ProcessingContext is the necessary data that a batch needs to provide to the runtime,
// without the historical state data (processing receipt from previous batch)
type ProcessingContext struct {
	BatchNumber    uint64
	Coinbase       common.Address
	Timestamp      time.Time
	GlobalExitRoot common.Hash
	ForcedBatchNum *uint64
}

// ProcessingReceipt indicates the outcome (StateRoot, AccInputHash) of processing a batch
type ProcessingReceipt struct {
	BatchNumber   uint64
	StateRoot     common.Hash
	LocalExitRoot common.Hash
	AccInputHash  common.Hash
	// Txs           []types.Transaction
	BatchL2Data []byte
}

// VerifiedBatch represents a VerifiedBatch
type VerifiedBatch struct {
	BlockNumber uint64
	BatchNumber uint64
	Aggregator  common.Address
	TxHash      common.Hash
	StateRoot   common.Hash
	IsTrusted   bool
}

// VirtualBatch represents a VirtualBatch
type VirtualBatch struct {
	BatchNumber   uint64
	TxHash        common.Hash
	Coinbase      common.Address
	SequencerAddr common.Address
	BlockNumber   uint64
}

// Sequence represents the sequence interval
type Sequence struct {
	FromBatchNumber uint64
	ToBatchNumber   uint64
}

type ProofHash struct {
	BlockNumber   uint64
	Sender        common.Address
	InitNumBatch  uint64
	FinalNewBatch uint64
	ProofHash     common.Hash
}

type ProverProof struct {
	InitNumBatch  uint64
	FinalNewBatch uint64
	NewStateRoot  common.Hash
	LocalExitRoot common.Hash
	Proof         string
	ProofHash     common.Hash
}

type FinalProof struct {
	MonitoredId  string
	FinalProof   string
	FinalProofId string
	CreatedAt    time.Time
	// updatedAt last date time it was updated
	UpdatedAt time.Time
}

package aggregator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/0xPolygonHermez/zkevm-node/aggregator/metrics"
	"github.com/0xPolygonHermez/zkevm-node/aggregator/pb"
	"github.com/0xPolygonHermez/zkevm-node/aggregator/prover"
	"github.com/0xPolygonHermez/zkevm-node/config/types"
	"github.com/0xPolygonHermez/zkevm-node/encoding"
	ethmanTypes "github.com/0xPolygonHermez/zkevm-node/etherman/types"
	"github.com/0xPolygonHermez/zkevm-node/ethtxmanager"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/state"
	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v4"
	"google.golang.org/grpc"
	grpchealth "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"

	"github.com/ethereum/go-ethereum/crypto"
	solsha3 "github.com/miguelmota/go-solidity-sha3"
)

const (
	mockedStateRoot     = "0x090bcaf734c4f06c93954a827b45a6e8c67b8e0fd1e0a35a1c5982d6961828f9"
	mockedLocalExitRoot = "0x17c04c3760510b48c6012742c540a81aba4bca2f78b9d14bfd2f123e2e53ea3e"

	ethTxManagerOwner     = "aggregator"
	monitoredIDFormat     = "proof-from-%v-to-%v"
	monitoredHashIDFormat = "proof-hash-from-%v-to-%v"

	max_commit_proof = 10
)

type finalProofMsg struct {
	proverName     string
	proverID       string
	recursiveProof *state.Proof
	finalProof     *pb.FinalProof
}

type proofHash struct {
	hash             string
	batchNumberFinal uint64
}

// Aggregator represents an aggregator
type Aggregator struct {
	pb.UnimplementedAggregatorServiceServer

	cfg Config

	State                       stateInterface
	EthTxManager                ethTxManager
	Ethman                      etherman
	ProfitabilityChecker        aggregatorTxProfitabilityChecker
	TimeSendFinalProof          time.Time
	TimeSendFinalProofHash      time.Time
	TimeCleanupLockedProofs     types.Duration
	StateDBMutex                *sync.Mutex
	TimeSendFinalProofMutex     *sync.RWMutex
	TimeSendFinalProofHashMutex *sync.RWMutex

	finalProof         chan finalProofMsg
	proofHashCH        chan proofHash
	verifyingProof     bool
	verifyingProofHash bool

	srv  *grpc.Server
	ctx  context.Context
	exit context.CancelFunc

	buildFinalProofBatchNumMutex *sync.Mutex
	buildFinalProofBatchNum      uint64

	monitoredProofHashTxLock *sync.Mutex
	monitoredProofHashTx     map[string]bool
}

// New creates a new aggregator.
func New(
	cfg Config,
	stateInterface stateInterface,
	ethTxManager ethTxManager,
	etherman etherman,
) (Aggregator, error) {
	var profitabilityChecker aggregatorTxProfitabilityChecker
	switch cfg.TxProfitabilityCheckerType {
	case ProfitabilityBase:
		profitabilityChecker = NewTxProfitabilityCheckerBase(stateInterface, cfg.IntervalAfterWhichBatchConsolidateAnyway.Duration, cfg.TxProfitabilityMinReward.Int)
	case ProfitabilityAcceptAll:
		profitabilityChecker = NewTxProfitabilityCheckerAcceptAll(stateInterface, cfg.IntervalAfterWhichBatchConsolidateAnyway.Duration)
	}

	a := Aggregator{
		cfg: cfg,

		State:                        stateInterface,
		EthTxManager:                 ethTxManager,
		Ethman:                       etherman,
		ProfitabilityChecker:         profitabilityChecker,
		StateDBMutex:                 &sync.Mutex{},
		TimeSendFinalProofMutex:      &sync.RWMutex{},
		TimeSendFinalProofHashMutex:  &sync.RWMutex{},
		buildFinalProofBatchNumMutex: &sync.Mutex{},
		TimeCleanupLockedProofs:      cfg.CleanupLockedProofsInterval,

		finalProof:               make(chan finalProofMsg, 10240),
		proofHashCH:              make(chan proofHash, 10240),
		monitoredProofHashTxLock: &sync.Mutex{},
		monitoredProofHashTx:     make(map[string]bool),
	}

	return a, nil
}

// Start starts the aggregator
func (a *Aggregator) Start(ctx context.Context) error {
	var cancel context.CancelFunc
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel = context.WithCancel(ctx)
	a.ctx = ctx
	a.exit = cancel

	metrics.Register()

	// process monitored batch verifications before starting
	a.EthTxManager.ProcessPendingMonitoredTxs(ctx, ethTxManagerOwner, func(result ethtxmanager.MonitoredTxResult, dbTx pgx.Tx) {
		a.handleMonitoredTxResult(result)
	}, nil)

	// Delete ungenerated recursive proofs
	err := a.State.DeleteUngeneratedProofs(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to initialize proofs cache %v", err)
	}

	go a.resendProoHash()

	address := fmt.Sprintf("%s:%d", a.cfg.Host, a.cfg.Port)
	lis, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	a.srv = grpc.NewServer()
	pb.RegisterAggregatorServiceServer(a.srv, a)

	healthService := newHealthChecker()
	grpchealth.RegisterHealthServer(a.srv, healthService)

	go func() {
		log.Infof("Server listening on port %d", a.cfg.Port)
		if err := a.srv.Serve(lis); err != nil {
			a.exit()
			log.Fatalf("Failed to serve: %v", err)
		}
	}()

	a.resetVerifyProofTime()

	go a.cleanupLockedProofs()
	go a.sendFinalProof()

	<-ctx.Done()
	return ctx.Err()
}

func (a *Aggregator) resendProoHash() {
	blockNumber := uint64(0)
	lastBatchNum := uint64(0)
	for {
		select {
		case <-a.ctx.Done():
			return
		default:
		}

		curBlockNumber, err := a.Ethman.GetLatestBlockNumber(a.ctx)
		log.Infof("curBlockNumber : %d", curBlockNumber)
		if err != nil {
			log.Errorf("Failed get last block by jsonrpc: %v", err)
			continue
		}

		if blockNumber > 0 && (blockNumber+1) > curBlockNumber {
			time.Sleep(3 * time.Second)
			continue
		}

		blockNumber = curBlockNumber

		lastVerifiedEthBatchNum, err := a.Ethman.GetLatestVerifiedBatchNum()
		log.Infof("lastVerifiedEthBatchNum : %d", lastVerifiedEthBatchNum)
		if err != nil {
			log.Warnf("Failed to get last eth batch on resendProoHash, err: %v", err)
			continue
		}
		tmp := lastVerifiedEthBatchNum
		for {
			sequence, err := a.State.GetSequence(a.ctx, tmp+1, nil)
			if err != nil {
				if errors.Is(err, state.ErrStateNotSynchronized) {
					log.Debugf("no transaction. err; %v", err)
					break
				}
				log.Warnf("failed to get sequence. err: %v", err)
				continue
			}

			if lastBatchNum > 0 && lastBatchNum > sequence.ToBatchNumber {
				continue
			}

			lastBatchNum = sequence.ToBatchNumber

			log.Infof("sequence : %v", sequence)

			monitoredProofTxID := buildMonitoredTxID(sequence.FromBatchNumber, sequence.ToBatchNumber)
			monitoredProofhashTxID := fmt.Sprintf(monitoredHashIDFormat, sequence.FromBatchNumber, sequence.ToBatchNumber)

			_, status, err := a.State.GetTxBlockNum(a.ctx, monitoredProofTxID, nil)
			if err != nil && err != state.ErrNotFound {
				log.Errorf("failed to get tx block number. monitoredTxID = %s, err = %v", monitoredProofTxID, err)
			}

			if err == nil && status != string(ethtxmanager.MonitoredTxStatusFailed) {
				tmp++
				continue
			}

			if err == nil {
				if err := a.EthTxManager.UpdateId(a.ctx, monitoredProofTxID, nil); err != nil {
					log.Errorf("failed to update monitoted tx id. err: %v. monitoredProofTxID: %s", err, monitoredProofTxID)
					continue
				}
			}

			_, _, err = a.State.GetTxBlockNum(a.ctx, monitoredProofhashTxID, nil)
			if err != nil {
				log.Errorf("failed to get tx block number. monitoredTxID = %s, err = %v", monitoredProofhashTxID, err)
				tmp++
				continue
			}

			firstProofHashBlockNumber, err := a.Ethman.GetSequencedBatch(sequence.ToBatchNumber)
			if err != nil {
				log.Errorf("failed to query first proof hash block number. err: %v, monitoredProofTxID: %s", err, monitoredProofTxID)
				continue
			}

			if firstProofHashBlockNumber == 0 {
				continue
			}

			log.Infof("proofHashTxBlockNumber : %v, monitoredTxID: %s", firstProofHashBlockNumber, monitoredProofhashTxID)

			if (firstProofHashBlockNumber + 20) > curBlockNumber {
				if (firstProofHashBlockNumber + 10) < curBlockNumber {
					a.monitoredProofHashTxLock.Lock()
					if _, ok := a.monitoredProofHashTx[monitoredProofhashTxID]; !ok {
						a.monitoredProofHashTx[monitoredProofhashTxID] = true
						go a.monitorSendProof(sequence.FromBatchNumber, sequence.ToBatchNumber, monitoredProofhashTxID)
					}
					a.monitoredProofHashTxLock.Unlock()
				}
				log.Debugf("no resend. proofHashTxBlockNumber = %d, curBlockNumber = %d", firstProofHashBlockNumber, curBlockNumber)
				tmp++
				continue
			}

			a.monitoredProofHashTxLock.Lock()
			if _, ok := a.monitoredProofHashTx[monitoredProofhashTxID]; !ok {
				a.monitoredProofHashTx[monitoredProofhashTxID] = true
			}
			a.monitoredProofHashTxLock.Unlock()
			if err := a.EthTxManager.UpdateId(a.ctx, monitoredProofhashTxID, nil); err != nil {
				log.Error("failed to update monitored Proofhash tx id. monitoredProofhashTxID: %s, err: %v", monitoredProofhashTxID, err)
				continue
			}

			// dbTx, err := a.State.BeginStateTransaction(a.ctx)
			// if err != nil {
			// 	log.Errorf("failed to begin state transaction for resend. err: %v", err)
			// 	continue
			// }
			// log.Infof("resend proof hash tx begin. monitoredProofhashTxID: %d", monitoredProofhashTxID)
			// resend, err := a.EthTxManager.AddReSendTx(a.ctx, monitoredProofhashTxID, dbTx)
			// if err != nil {
			// 	if err := dbTx.Rollback(a.ctx); err != nil {
			// 		err := fmt.Errorf("failed to rollback resend: %v", err)
			// 		log.Error(FirstToUpper(err.Error()))
			// 		continue
			// 	}
			// 	log.Errorf("failed to release resend: %v", err)
			// 	continue
			// }

			// err = dbTx.Commit(a.ctx)
			// if err != nil {
			// 	log.Errorf("failed to release state transaction for resend %v", err)
			// 	continue
			// }

			// a.EthTxManager.ProcessPendingMonitoredTxs(a.ctx, ethTxManagerOwner, func(result ethtxmanager.MonitoredTxResult, dbTx pgx.Tx) {
			// 	if result.Status == ethtxmanager.MonitoredTxStatusFailed {
			// 		resultLog := log.WithFields("owner", ethTxManagerOwner, "id", result.ID)
			// 		resultLog.Error("failed to resend proof hash, TODO: review this fatal and define what to do in this case")
			// 		if err := a.EthTxManager.UpdateId(a.ctx, result.ID, nil); err != nil {
			// 			resultLog.Error(err)
			// 		}
			// 	}
			// }, nil)
			// if resend {
			// 	go a.monitorSendProof(sequence.FromBatchNumber, sequence.ToBatchNumber, monitoredProofhashTxID)
			// 	log.Infof("resend proof hash to opside chain. proofHashTxBlockNumber = %d, curBlockNumber = %d, monitoredProofhashTxID = %s", firstProofHashBlockNumber, curBlockNumber, monitoredProofhashTxID)
			// }

			log.Infof("resend proof hash tx end. monitoredProofhashTxID: %d", monitoredProofhashTxID)
			tmp++
		}
	}
}

// Stop stops the Aggregator server.
func (a *Aggregator) Stop() {
	a.exit()
	a.srv.Stop()
}

// Channel implements the bi-directional communication channel between the
// Prover client and the Aggregator server.
func (a *Aggregator) Channel(stream pb.AggregatorService_ChannelServer) error {
	metrics.ConnectedProver()
	defer metrics.DisconnectedProver()

	ctx := stream.Context()
	var proverAddr net.Addr
	p, ok := peer.FromContext(ctx)
	if ok {
		proverAddr = p.Addr
	}
	prover, err := prover.New(stream, proverAddr, a.cfg.ProofStatePollingInterval)
	if err != nil {
		return err
	}

	log := log.WithFields(
		"prover", prover.Name(),
		"proverId", prover.ID(),
		"proverAddr", prover.Addr(),
	)
	log.Info("Establishing stream connection with prover")

	// Check if prover supports the required Fork ID
	if !prover.SupportsForkID(a.cfg.ForkId) {
		err := errors.New("prover does not support required fork ID")
		log.Warn(FirstToUpper(err.Error()))
		return err
	}

	for {
		select {
		case <-a.ctx.Done():
			// server disconnected
			return a.ctx.Err()
		case <-ctx.Done():
			// client disconnected
			return ctx.Err()

		default:
			depoist, err := a.Ethman.JudgeAggregatorDeposit(common.HexToAddress(a.cfg.SenderAddress))
			if !depoist {
				if err != nil {
					log.Errorf("Failed to get deposit acount: %v", err)
				} else {
					log.Debugf("Check deposit amount. senderAddress: %s", a.cfg.SenderAddress)
				}

				time.Sleep(a.cfg.RetryTime.Duration)
				continue
			}

			isIdle, err := prover.IsIdle()
			if err != nil {
				log.Errorf("Failed to check if prover is idle: %v", err)
				time.Sleep(a.cfg.RetryTime.Duration)
				continue
			}
			if !isIdle {
				log.Debug("Prover is not idle")
				time.Sleep(a.cfg.RetryTime.Duration)
				continue
			}

			_, err = a.tryBuildFinalProof(ctx, prover, nil)
			if err != nil {
				log.Errorf("Error checking proofs to verify: %v", err)
			}

			proofGenerated, err := a.tryAggregateProofs(ctx, prover)
			if err != nil {
				log.Errorf("Error trying to aggregate proofs: %v", err)
			}
			if !proofGenerated {
				proofGenerated, err = a.tryGenerateBatchProof(ctx, prover)
				if err != nil {
					log.Errorf("Error trying to generate proof: %v", err)
				}
			}
			if !proofGenerated {
				// if no proof was generated (aggregated or batch) wait some time before retry
				time.Sleep(a.cfg.RetryTime.Duration)
			} // if proof was generated we retry immediately as probably we have more proofs to process
		}
	}
}

type finalProofMsgList []finalProofMsg

func (h finalProofMsgList) Len() int { return len(h) }
func (h finalProofMsgList) Less(i, j int) bool {
	return h[i].recursiveProof.BatchNumberFinal < h[j].recursiveProof.BatchNumberFinal
}
func (h finalProofMsgList) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

// This function waits to receive a final proof from a prover. Once it receives
// the proof, it performs these steps in order:
// - send the final proof to L1
// - wait for the synchronizer to catch up
// - clean up the cache of recursive proofs
func (a *Aggregator) sendFinalProof() {
	var lock sync.Mutex
	finalProofMsgs := make(finalProofMsgList, 0)
	tick := time.NewTicker(time.Second * 1)
	blockNumber := uint64(0)
	for {
		select {
		case <-a.ctx.Done():
			return
		case msg := <-a.finalProof:
			a.monitoredProofHashTxLock.Lock()
			monitoredTxID := fmt.Sprintf(monitoredHashIDFormat, msg.recursiveProof.BatchNumber, msg.recursiveProof.BatchNumberFinal)
			if _, ok := a.monitoredProofHashTx[monitoredTxID]; ok {
				continue
			}
			a.monitoredProofHashTxLock.Unlock()
			lock.Lock()
			finalProofMsgs = append(finalProofMsgs, msg)
			sort.Sort(finalProofMsgs)
			lock.Unlock()

		case <-tick.C:
			if finalProofMsgs.Len() > 0 {
				curBlockNumber, err := a.Ethman.GetLatestBlockNumber(a.ctx)
				log.Infof("curBlockNumber : %d", curBlockNumber)
				if err != nil {
					log.Errorf("Failed get last block by jsonrpc: %v", err)
					continue
				}

				if blockNumber > 0 && (blockNumber+1) > curBlockNumber {
					time.Sleep(3 * time.Second)
					continue
				}

				blockNumber = curBlockNumber

				lock.Lock()
				msg := finalProofMsgs[0]

				if finalProofMsgs.Len() > 1 {
					finalProofMsgs = finalProofMsgs[1:]
				} else {
					finalProofMsgs = make(finalProofMsgList, 0)
				}
				lock.Unlock()
				ctx := a.ctx
				proof := msg.recursiveProof
				log.WithFields("proofId", proof.ProofID, "batches", fmt.Sprintf("%d-%d", proof.BatchNumber, proof.BatchNumberFinal))

				sha3 := solsha3.SoliditySHA3(msg.finalProof.Proof)
				pack := solsha3.Pack([]string{"string", "address"}, []interface{}{
					sha3,
					common.HexToAddress(a.cfg.SenderAddress),
				})

				hash := crypto.Keccak256Hash(pack)
				monitoredTxID := fmt.Sprintf(monitoredHashIDFormat, proof.BatchNumber, proof.BatchNumberFinal)
				a.monitoredProofHashTxLock.Lock()
				if _, ok := a.monitoredProofHashTx[monitoredTxID]; !ok {
					a.monitoredProofHashTx[monitoredTxID] = true
				}
				a.monitoredProofHashTxLock.Unlock()

				a.startProofHash()

				finalBatch, err := a.State.GetBatchByNumber(ctx, proof.BatchNumberFinal, nil)
				if err != nil {
					log.Errorf("Failed to retrieve batch with number [%d]: %v", proof.BatchNumberFinal, err)
					a.endProofHash()
					continue
				}

				proofHashBlockNum, err := a.Ethman.GetSequencedBatch(proof.BatchNumberFinal)
				if err != nil {
					log.Errorf("failed to get block number for first proof hash")
					continue
				}
				if proofHashBlockNum > 0 {
					block, err := a.State.GetLastBlock(a.ctx, nil)
					if err != nil {
						log.Errorf("Error get last block: %v", err)
						a.endProofHash()
						a.handleFailureToAddVerifyBatchToBeMonitored(ctx, proof)
						continue
					}

					if (block.BlockNumber-proofHashBlockNum) > max_commit_proof && (block.BlockNumber-proofHashBlockNum) <= 2*max_commit_proof {
						log.Debugf("not cant sender proof hash. proofHashBlockNum: %d, current block number", proofHashBlockNum, block.BlockNumber)
						continue
					}
				}

				// query
				to, data, err := a.Ethman.BuildProofHashTxData(proof.BatchNumber-1, proof.BatchNumberFinal, hash)
				if err != nil {
					log.Errorf("Error estimating proof hash to add to eth tx manager: %v", err)
					a.endProofHash()
					a.handleFailureToAddVerifyBatchToBeMonitored(ctx, proof)
					continue
				}

				sender := common.HexToAddress(a.cfg.SenderAddress)
				err = a.EthTxManager.Add(ctx, ethTxManagerOwner, monitoredTxID, sender, to, nil, data, nil)
				if err != nil {
					log := log.WithFields("tx", monitoredTxID)
					log.Errorf("Error to add batch verification tx to eth tx manager: %v", err)

					a.endProofHash()
					a.handleFailureToAddVerifyBatchToBeMonitored(ctx, proof)
					continue
				}

				a.EthTxManager.ProcessPendingMonitoredTxs(ctx, ethTxManagerOwner, func(result ethtxmanager.MonitoredTxResult, dbTx pgx.Tx) {
					if result.Status == ethtxmanager.MonitoredTxStatusFailed {
						resultLog := log.WithFields("owner", ethTxManagerOwner, "id", result.ID)
						resultLog.Error("failed to send proof hash, TODO: review this fatal and define what to do in this case")
						if err := a.EthTxManager.UpdateId(a.ctx, result.ID, nil); err != nil {
							resultLog.Error(err)
						}
					}
				}, nil)

				proverProof, err := a.State.GetProverProofByHash(a.ctx, hash.String(), proof.BatchNumberFinal, nil)
				log.Infof("monitoredTxID = %s, hash = %s, proverProof = %v", monitoredTxID, hash.String(), proverProof)
				if err != nil || proverProof == nil {
					if err := a.State.AddProverProof(a.ctx, &state.ProverProof{
						InitNumBatch:  proof.BatchNumber,
						FinalNewBatch: proof.BatchNumberFinal,
						NewStateRoot:  finalBatch.StateRoot,
						LocalExitRoot: finalBatch.LocalExitRoot,
						Proof:         msg.finalProof.Proof,
						ProofHash:     hash,
					}, nil); err != nil {
						log := log.WithFields("tx", monitoredTxID)
						log.Errorf("Error to add prover proof to db: %v", err)
						a.endProofHash()
						a.handleFailureToAddVerifyBatchToBeMonitored(ctx, proof)
						continue
					}
				}

				a.resetVerifyProofHashTime()
				a.endProofHash()

				go a.monitorSendProof(proof.BatchNumber, proof.BatchNumberFinal, monitoredTxID)
			}
		case proofHash := <-a.proofHashCH:
			proverProof, err := a.State.GetProverProofByHash(a.ctx, proofHash.hash, proofHash.batchNumberFinal, nil)
			if err != nil {
				log.Errorf("Error to get prover proof: %v", err)
				a.proofHashCH <- proofHash
				continue
			}

			log.WithFields("batches", fmt.Sprintf("%d-%d", proverProof.InitNumBatch, proverProof.FinalNewBatch))
			log.Info("Verifying final proof with ethereum smart contract")

			a.startProofVerification()

			inputs := ethmanTypes.FinalProofInputs{
				FinalProof:       &pb.FinalProof{Proof: proverProof.Proof},
				NewLocalExitRoot: proverProof.LocalExitRoot.Bytes(),
				NewStateRoot:     proverProof.NewStateRoot.Bytes(),
			}

			log.Infof("Final proof inputs: NewLocalExitRoot [%#x], NewStateRoot [%#x]", inputs.NewLocalExitRoot, inputs.NewStateRoot)

			// add batch verification to be monitored
			sender := common.HexToAddress(a.cfg.SenderAddress)

			to, data, err := a.Ethman.BuildUnTrustedVerifyBatchesTxData(proverProof.InitNumBatch-1, proverProof.FinalNewBatch, &inputs)
			if err != nil {
				log.Errorf("Error estimating batch verification to add to eth tx manager: %v", err)
				//a.handleFailureToAddVerifyBatchToBeMonitored(ctx, proof)
				a.endProofVerification()
				continue
			}

			monitoredTxID := buildMonitoredTxID(proverProof.InitNumBatch, proverProof.FinalNewBatch)
			err = a.EthTxManager.Add(a.ctx, ethTxManagerOwner, monitoredTxID, sender, to, nil, data, nil)
			if err != nil {
				log := log.WithFields("tx", monitoredTxID)
				log.Errorf("Error to add batch verification tx to eth tx manager: %v", err)
				a.endProofVerification()
				continue
			}

			// process monitored batch verifications before starting a next cycle
			a.EthTxManager.ProcessPendingMonitoredTxs(a.ctx, ethTxManagerOwner, func(result ethtxmanager.MonitoredTxResult, dbTx pgx.Tx) {
				a.handleMonitoredTxResult(result)
			}, nil)

			a.resetVerifyProofTime()
			a.endProofVerification()

			a.monitoredProofHashTxLock.Lock()
			if b, ok := a.monitoredProofHashTx[monitoredTxID]; ok && b {
				delete(a.monitoredProofHashTx, monitoredTxID)
			}
			a.monitoredProofHashTxLock.Unlock()

			// case msg := <-a.finalProof:
			// 	ctx := a.ctx
			// 	proof := msg.recursiveProof

			// 	log.WithFields("proofId", proof.ProofID, "batches", fmt.Sprintf("%d-%d", proof.BatchNumber, proof.BatchNumberFinal))
			// 	log.Info("Verifying final proof with ethereum smart contract")

			// 	a.startProofVerification()

			// 	finalBatch, err := a.State.GetBatchByNumber(ctx, proof.BatchNumberFinal, nil)
			// 	if err != nil {
			// 		log.Errorf("Failed to retrieve batch with number [%d]: %v", proof.BatchNumberFinal, err)
			// 		a.endProofVerification()
			// 		continue
			// 	}

			// 	inputs := ethmanTypes.FinalProofInputs{
			// 		FinalProof:       msg.finalProof,
			// 		NewLocalExitRoot: finalBatch.LocalExitRoot.Bytes(),
			// 		NewStateRoot:     finalBatch.StateRoot.Bytes(),
			// 	}

			// 	log.Infof("Final proof inputs: NewLocalExitRoot [%#x], NewStateRoot [%#x]", inputs.NewLocalExitRoot, inputs.NewStateRoot)

			// 	// add batch verification to be monitored
			// 	sender := common.HexToAddress(a.cfg.SenderAddress)
			// 	to, data, err := a.Ethman.BuildTrustedVerifyBatchesTxData(proof.BatchNumber-1, proof.BatchNumberFinal, &inputs)
			// 	if err != nil {
			// 		log.Errorf("Error estimating batch verification to add to eth tx manager: %v", err)
			// 		a.handleFailureToAddVerifyBatchToBeMonitored(ctx, proof)
			// 		continue
			// 	}
			// 	monitoredTxID := buildMonitoredTxID(proof.BatchNumber, proof.BatchNumberFinal)
			// 	err = a.EthTxManager.Add(ctx, ethTxManagerOwner, monitoredTxID, sender, to, nil, data, nil)
			// 	if err != nil {
			// 		log := log.WithFields("tx", monitoredTxID)
			// 		log.Errorf("Error to add batch verification tx to eth tx manager: %v", err)
			// 		a.handleFailureToAddVerifyBatchToBeMonitored(ctx, proof)
			// 		continue
			// 	}

			// 	// process monitored batch verifications before starting a next cycle
			// 	a.EthTxManager.ProcessPendingMonitoredTxs(ctx, ethTxManagerOwner, func(result ethtxmanager.MonitoredTxResult, dbTx pgx.Tx) {
			// 		a.handleMonitoredTxResult(result)
			// 	}, nil)

			// 	a.resetVerifyProofTime()
			// 	a.endProofVerification()
		}
	}
}

func (a *Aggregator) monitorSendProof(batchNumber, batchNumberFinal uint64, monitoredTxID string) {
	tick := time.NewTicker(time.Second * 10)
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-tick.C:
			blockNumber, err := a.Ethman.GetLatestBlockNumber(a.ctx)
			if err != nil {
				log.Errorf("Failed get last block by jsonrpc: %v", err)
				continue
			}

			lastVerifiedEthBatchNum, err := a.Ethman.GetLatestVerifiedBatchNum()
			log.Infof("monitorSendProof lastVerifiedEthBatchNum : %d", lastVerifiedEthBatchNum)
			if err != nil {
				log.Warnf("Failed to get last eth batch on monitorSendProof, err: %v", err)
				continue
			}

			if (lastVerifiedEthBatchNum + 1) != batchNumber {
				log.Debugf("lastVerifiedEthBatchNum: %d, initBatchNum: %d", lastVerifiedEthBatchNum, batchNumber)
				continue
			}

			proofHashBlockNum, err := a.Ethman.GetSequencedBatch(batchNumberFinal)
			if err != nil {
				log.Errorf("failed to get block number for first proof hash")
				continue
			}

			log.Infof("proofHashBlockNum = %d, max_commit_proof = %d, blockNumber =%d, monitoredTxID = %s", proofHashBlockNum, max_commit_proof, blockNumber, monitoredTxID)
			if proofHashBlockNum+max_commit_proof > blockNumber {
				continue
			}

			hash, err := a.State.GetProofHashBySender(a.ctx, a.cfg.SenderAddress, batchNumberFinal, max_commit_proof, blockNumber, nil)
			if err != nil {
				if err == state.ProofNotCommit {
					log.Errorf("batchNumberFinal  = %d, error: %v", batchNumberFinal, err)
					return
				}
				log.Debugf("Failed get proof hash in monitorSendProof: %v, batchNumberFinal: %d", err, batchNumberFinal)
				continue
			}
			log.Infof("build proof tx. hash: %s, batchNumberFinal: %d, monitoredTxID = %s", hash, batchNumberFinal, monitoredTxID)
			a.proofHashCH <- proofHash{hash, batchNumberFinal}
			return
		}
	}
}

func (a *Aggregator) handleFailureToAddVerifyBatchToBeMonitored(ctx context.Context, proof *state.Proof) {
	log := log.WithFields("proofId", proof.ProofID, "batches", fmt.Sprintf("%d-%d", proof.BatchNumber, proof.BatchNumberFinal))
	proof.GeneratingSince = nil
	err := a.State.UpdateGeneratedProof(ctx, proof, nil)
	if err != nil {
		log.Errorf("Failed updating proof state (false): %v", err)
	}
	a.endProofVerification()
}

// buildFinalProof builds and return the final proof for an aggregated/batch proof.
func (a *Aggregator) buildFinalProof(ctx context.Context, prover proverInterface, proof *state.Proof) (*pb.FinalProof, error) {
	log := log.WithFields(
		"prover", prover.Name(),
		"proverId", prover.ID(),
		"proverAddr", prover.Addr(),
		"recursiveProofId", *proof.ProofID,
		"batches", fmt.Sprintf("%d-%d", proof.BatchNumber, proof.BatchNumberFinal),
	)
	log.Info("Generating final proof")

	finalProofID, err := prover.FinalProof(proof.Proof, a.cfg.SenderAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to get final proof id: %v", err)
	}
	proof.ProofID = finalProofID

	log.Infof("Final proof ID for batches [%d-%d]: %s", proof.BatchNumber, proof.BatchNumberFinal, *proof.ProofID)
	log = log.WithFields("finalProofId", finalProofID)

	finalProof, err := prover.WaitFinalProof(ctx, *proof.ProofID)
	if err != nil {
		return nil, fmt.Errorf("failed to get final proof from prover: %v", err)
	}

	monitoredTxID := fmt.Sprintf(monitoredHashIDFormat, proof.BatchNumber, proof.BatchNumberFinal)

	stateFinalProof := state.FinalProof{
		MonitoredId:  monitoredTxID,
		FinalProof:   finalProof.Proof,
		FinalProofId: *finalProofID,
	}

	if err := a.State.AddFinalProof(a.ctx, &stateFinalProof, nil); err != nil {
		log.Error("failed to add final proof. state-monitoredTxID: %s, err = %v", monitoredTxID, err)
	}

	log.Info("Final proof generated")

	// mock prover sanity check
	if string(finalProof.Public.NewStateRoot) == mockedStateRoot && string(finalProof.Public.NewLocalExitRoot) == mockedLocalExitRoot {
		// This local exit root and state root come from the mock
		// prover, use the one captured by the executor instead
		finalBatch, err := a.State.GetBatchByNumber(ctx, proof.BatchNumberFinal, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve batch with number [%d]", proof.BatchNumberFinal)
		}
		log.Warnf("NewLocalExitRoot and NewStateRoot look like a mock values, using values from executor instead: LER: %v, SR: %v",
			finalBatch.LocalExitRoot.TerminalString(), finalBatch.StateRoot.TerminalString())
		finalProof.Public.NewStateRoot = finalBatch.StateRoot.Bytes()
		finalProof.Public.NewLocalExitRoot = finalBatch.LocalExitRoot.Bytes()
	}

	return finalProof, nil
}

// tryBuildFinalProof checks if the provided proof is eligible to be used to
// build the final proof.  If no proof is provided it looks for a previously
// generated proof.  If the proof is eligible, then the final proof generation
// is triggered.
func (a *Aggregator) tryBuildFinalProof(ctx context.Context, prover proverInterface, proof *state.Proof) (bool, error) {
	proverName := prover.Name()
	proverID := prover.ID()

	log := log.WithFields(
		"prover", proverName,
		"proverId", proverID,
		"proverAddr", prover.Addr(),
	)
	log.Debug("tryBuildFinalProof start")

	var err error
	// if !a.canVerifyProof() {
	// 	log.Debug("Time to verify proof not reached or proof verification in progress")
	// 	return false, nil
	// }
	if !a.canVerifyProofHash() {
		log.Debug("Time to verify proof hash not reached or proof verification in progress")
		return false, nil
	}
	log.Debug("Send final proof hash time reached")

	for !a.isSynced(ctx, nil) {
		log.Info("Waiting for synchronizer to sync...")
		time.Sleep(a.cfg.RetryTime.Duration)
		continue
	}

	var lastVerifiedBatchNum uint64
	lastVerifiedBatch, err := a.State.GetLastVerifiedBatch(ctx, nil)
	if err != nil && !errors.Is(err, state.ErrNotFound) {
		return false, fmt.Errorf("failed to get last verified batch, %v", err)
	}
	if lastVerifiedBatch != nil {
		lastVerifiedBatchNum = lastVerifiedBatch.BatchNumber
	}

	var msg finalProofMsg

	if proof == nil {
		// we don't have a proof generating at the moment, check if we
		// have a proof ready to verify
		proof, err = a.getAndLockProofReadyToVerify(ctx, prover, lastVerifiedBatchNum)
		if errors.Is(err, state.ErrNotFound) {
			// nothing to verify, swallow the error
			log.Debugf("No proof ready to verify. lastVerifiedBatchNum: %d", lastVerifiedBatchNum)
			return false, nil
		}

		if err != nil {
			log.Errorf("failed to get and lock proof ready to verify. err: %v", err)
			return false, err
		}

		monitoredTxID := fmt.Sprintf(monitoredHashIDFormat, proof.BatchNumber, proof.BatchNumberFinal)
		stateFinalProof, errFinalProof := a.State.GetFinalProofByMonitoredId(a.ctx, monitoredTxID, nil)
		if errFinalProof != nil && errFinalProof != state.ErrNotFound {
			log.Errorf("failed to read finalProof from table. err: %v", errFinalProof)
			return false, errFinalProof
		}

		msg = finalProofMsg{
			proverName: proverName,
			proverID:   proverID,
		}

		if errFinalProof == state.ErrNotFound {
			// at this point we have an eligible proof, build the final one using it
			finalProof, err := a.buildFinalProof(ctx, prover, proof)
			if err != nil {
				err = fmt.Errorf("failed to build final proof, %v", err)
				log.Error(FirstToUpper(err.Error()))
				return false, err
			}

			msg.recursiveProof = proof
			msg.finalProof = finalProof
		} else {
			proof.ProofID = &stateFinalProof.FinalProofId
			msg.recursiveProof = proof
			msg.finalProof = &pb.FinalProof{Proof: stateFinalProof.FinalProof}
		}

		defer func() {
			if err != nil {
				// Set the generating state to false for the proof ("unlock" it)
				proof.GeneratingSince = nil
				err2 := a.State.UpdateGeneratedProof(a.ctx, proof, nil)
				if err2 != nil {
					log.Errorf("Failed to unlock proof: %v", err2)
				}
			}
		}()
	} else {
		// we do have a proof generating at the moment, check if it is
		// eligible to be verified
		eligible, generate, err := a.validateEligibleFinalProof(ctx, proof, lastVerifiedBatchNum)
		if err != nil {
			return false, fmt.Errorf("failed to validate eligible final proof, %v", err)
		}

		if !eligible {
			if generate {
				// at this point we have an eligible proof, build the final one using it
				_, err := a.buildFinalProof(ctx, prover, proof)
				if err != nil {
					err = fmt.Errorf("failed to build final proof, %v", err)
					log.Error(FirstToUpper(err.Error()))
					return false, err
				}
			}
			return false, nil
		}

		monitoredTxID := fmt.Sprintf(monitoredHashIDFormat, proof.BatchNumber, proof.BatchNumberFinal)
		log.Infof("GetFinalProofByMonitoredId: %s", monitoredTxID)
		stateFinalProof, err := a.State.GetFinalProofByMonitoredId(a.ctx, monitoredTxID, nil)
		if err != nil && err != state.ErrNotFound {
			log.Errorf("failed to read finalProof from table. err: %v", err)
			return false, err
		}

		log = log.WithFields(
			"proofId", *proof.ProofID,
			"batches", fmt.Sprintf("%d-%d", proof.BatchNumber, proof.BatchNumberFinal),
		)
		if err == state.ErrNotFound {
			// at this point we have an eligible proof, build the final one using it
			finalProof, err := a.buildFinalProof(ctx, prover, proof)
			if err != nil {
				err = fmt.Errorf("failed to build final proof, %v", err)
				log.Error(FirstToUpper(err.Error()))
				return false, err
			}

			msg = finalProofMsg{
				proverName:     proverName,
				proverID:       proverID,
				recursiveProof: proof,
				finalProof:     finalProof,
			}
		} else {
			proof.ProofID = &stateFinalProof.FinalProofId
			msg.recursiveProof = proof
			msg.finalProof = &pb.FinalProof{Proof: stateFinalProof.FinalProof}
		}
	}

	select {
	case <-a.ctx.Done():
		return false, a.ctx.Err()
	case a.finalProof <- msg:
	}

	log.Debug("tryBuildFinalProof end")
	return true, nil
}

func (a *Aggregator) validateEligibleFinalProof(ctx context.Context, proof *state.Proof, lastVerifiedBatchNum uint64) (bool, bool, error) {
	batchNumberToVerify := lastVerifiedBatchNum + 1

	if proof.BatchNumber != batchNumberToVerify {
		if proof.BatchNumber < batchNumberToVerify && proof.BatchNumberFinal >= batchNumberToVerify {
			// We have a proof that contains some batches below the last batch verified, anyway can be eligible as final proof
			log.Warnf("Proof %d-%d contains some batches lower than last batch verified %d. Check anyway if it is eligible", proof.BatchNumber, proof.BatchNumberFinal, lastVerifiedBatchNum)
		} else if proof.BatchNumberFinal < batchNumberToVerify {
			// We have a proof that contains batches below that the last batch verified, we need to delete this proof
			log.Warnf("Proof %d-%d lower than next batch to verify %d. Deleting it", proof.BatchNumber, proof.BatchNumberFinal, batchNumberToVerify)
			err := a.State.DeleteGeneratedProofs(ctx, proof.BatchNumber, proof.BatchNumberFinal, nil)
			if err != nil {
				return false, false, fmt.Errorf("failed to delete discarded proof, err: %v", err)
			}
			return false, false, nil
		} else {
			log.Debugf("Proof batch number %d is not the following to last verfied batch number %d", proof.BatchNumber, lastVerifiedBatchNum)
			bComplete, err := a.State.CheckProofContainsCompleteSequences(ctx, proof, nil)
			if err != nil {
				return false, false, fmt.Errorf("failed to check if proof contains complete sequences, %v", err)
			}
			if !bComplete {
				log.Infof("Recursive proof %d-%d not eligible to be verified: not containing complete sequences", proof.BatchNumber, proof.BatchNumberFinal)
				return false, false, nil
			}

			return false, true, nil
		}
	}

	bComplete, err := a.State.CheckProofContainsCompleteSequences(ctx, proof, nil)
	if err != nil {
		return false, false, fmt.Errorf("failed to check if proof contains complete sequences, %v", err)
	}
	if !bComplete {
		log.Infof("Recursive proof %d-%d not eligible to be verified: not containing complete sequences", proof.BatchNumber, proof.BatchNumberFinal)
		return false, false, nil
	}
	return true, false, nil
}

func (a *Aggregator) getAndLockProofReadyToVerify(ctx context.Context, prover proverInterface, lastVerifiedBatchNum uint64) (*state.Proof, error) {
	a.StateDBMutex.Lock()
	defer a.StateDBMutex.Unlock()

	// Get proof ready to be verified
	proofToVerify, err := a.State.GetProofReadyToVerify(ctx, lastVerifiedBatchNum, nil)
	if err != nil {
		return nil, err
	}

	now := time.Now().Round(time.Microsecond)
	proofToVerify.GeneratingSince = &now

	err = a.State.UpdateGeneratedProof(ctx, proofToVerify, nil)
	if err != nil {
		return nil, err
	}

	return proofToVerify, nil
}

func (a *Aggregator) unlockProofsToAggregate(ctx context.Context, proof1 *state.Proof, proof2 *state.Proof) error {
	// Release proofs from generating state in a single transaction
	dbTx, err := a.State.BeginStateTransaction(ctx)
	if err != nil {
		log.Warnf("Failed to begin transaction to release proof aggregation state, err: %v", err)
		return err
	}

	proof1.GeneratingSince = nil
	err = a.State.UpdateGeneratedProof(ctx, proof1, dbTx)
	if err == nil {
		proof2.GeneratingSince = nil
		err = a.State.UpdateGeneratedProof(ctx, proof2, dbTx)
	}

	if err != nil {
		if err := dbTx.Rollback(ctx); err != nil {
			err := fmt.Errorf("failed to rollback proof aggregation state: %v", err)
			log.Error(FirstToUpper(err.Error()))
			return err
		}
		return fmt.Errorf("failed to release proof aggregation state: %v", err)
	}

	err = dbTx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("failed to release proof aggregation state %v", err)
	}

	return nil
}

func (a *Aggregator) getAndLockProofsToAggregate(ctx context.Context, prover proverInterface) (*state.Proof, *state.Proof, error) {
	log := log.WithFields(
		"prover", prover.Name(),
		"proverId", prover.ID(),
		"proverAddr", prover.Addr(),
	)

	a.StateDBMutex.Lock()
	defer a.StateDBMutex.Unlock()

	proof1, proof2, err := a.State.GetProofsToAggregate(ctx, nil)
	if err != nil {
		return nil, nil, err
	}

	// Set proofs in generating state in a single transaction
	dbTx, err := a.State.BeginStateTransaction(ctx)
	if err != nil {
		log.Errorf("Failed to begin transaction to set proof aggregation state, err: %v", err)
		return nil, nil, err
	}

	now := time.Now().Round(time.Microsecond)
	proof1.GeneratingSince = &now
	err = a.State.UpdateGeneratedProof(ctx, proof1, dbTx)
	if err == nil {
		proof2.GeneratingSince = &now
		err = a.State.UpdateGeneratedProof(ctx, proof2, dbTx)
	}

	if err != nil {
		if err := dbTx.Rollback(ctx); err != nil {
			err := fmt.Errorf("failed to rollback proof aggregation state %v", err)
			log.Error(FirstToUpper(err.Error()))
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("failed to set proof aggregation state %v", err)
	}

	err = dbTx.Commit(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to set proof aggregation state %v", err)
	}

	return proof1, proof2, nil
}

func (a *Aggregator) tryAggregateProofs(ctx context.Context, prover proverInterface) (bool, error) {
	proverName := prover.Name()
	proverID := prover.ID()

	log := log.WithFields(
		"prover", proverName,
		"proverId", proverID,
		"proverAddr", prover.Addr(),
	)
	log.Debug("tryAggregateProofs start")

	proof1, proof2, err0 := a.getAndLockProofsToAggregate(ctx, prover)
	if errors.Is(err0, state.ErrNotFound) {
		// nothing to aggregate, swallow the error
		log.Debug("Nothing to aggregate")
		return false, nil
	}
	if err0 != nil {
		return false, err0
	}

	var (
		aggrProofID *string
		err         error
	)

	defer func() {
		if err != nil {
			err2 := a.unlockProofsToAggregate(a.ctx, proof1, proof2)
			if err2 != nil {
				log.Errorf("Failed to release aggregated proofs, err: %v", err2)
			}
		}
		log.Debug("tryAggregateProofs end")
	}()

	log.Infof("Aggregating proofs: %d-%d and %d-%d", proof1.BatchNumber, proof1.BatchNumberFinal, proof2.BatchNumber, proof2.BatchNumberFinal)

	batches := fmt.Sprintf("%d-%d", proof1.BatchNumber, proof2.BatchNumberFinal)
	log = log.WithFields("batches", batches)

	inputProver := map[string]interface{}{
		"recursive_proof_1": proof1.Proof,
		"recursive_proof_2": proof2.Proof,
	}
	b, err := json.Marshal(inputProver)
	if err != nil {
		err = fmt.Errorf("failed to serialize input prover, %v", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	proof := &state.Proof{
		BatchNumber:      proof1.BatchNumber,
		BatchNumberFinal: proof2.BatchNumberFinal,
		Prover:           &proverName,
		ProverID:         &proverID,
		InputProver:      string(b),
	}

	aggrProofID, err = prover.AggregatedProof(proof1.Proof, proof2.Proof)
	if err != nil {
		err = fmt.Errorf("failed to get aggregated proof id, %v", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	proof.ProofID = aggrProofID

	log.Infof("Proof ID for aggregated proof: %v", *proof.ProofID)
	log = log.WithFields("proofId", *proof.ProofID)

	monitoredTxID := fmt.Sprintf(monitoredHashIDFormat, proof.BatchNumber, proof.BatchNumberFinal)
	_, err = a.State.GetFinalProofByMonitoredId(a.ctx, monitoredTxID, nil)
	if err != nil && err != state.ErrNotFound {
		log.Errorf("failed to read finalProof from table. err: %v", err)
		return false, err
	}
	if err == state.ErrNotFound {
		recursiveProof, err := prover.WaitRecursiveProof(ctx, *proof.ProofID)
		if err != nil {
			err = fmt.Errorf("failed to get aggregated proof from prover, %v", err)
			log.Error(FirstToUpper(err.Error()))
			return false, err
		}

		log.Info("Aggregated proof generated")
		proof.Proof = recursiveProof
	}

	// update the state by removing the 2 aggregated proofs and storing the
	// newly generated recursive proof
	dbTx, err := a.State.BeginStateTransaction(ctx)
	if err != nil {
		err = fmt.Errorf("failed to begin transaction to update proof aggregation state, %v", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	err = a.State.DeleteGeneratedProofs(ctx, proof1.BatchNumber, proof2.BatchNumberFinal, dbTx)
	if err != nil {
		if err := dbTx.Rollback(ctx); err != nil {
			err := fmt.Errorf("failed to rollback proof aggregation state, %v", err)
			log.Error(FirstToUpper(err.Error()))
			return false, err
		}
		err = fmt.Errorf("failed to delete previously aggregated proofs, %v", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	now := time.Now().Round(time.Microsecond)
	proof.GeneratingSince = &now

	err = a.State.AddGeneratedProof(ctx, proof, dbTx)
	if err != nil {
		if err := dbTx.Rollback(ctx); err != nil {
			err := fmt.Errorf("failed to rollback proof aggregation state, %v", err)
			log.Error(FirstToUpper(err.Error()))
			return false, err
		}
		err = fmt.Errorf("failed to store the recursive proof, %v", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	err = dbTx.Commit(ctx)
	if err != nil {
		err = fmt.Errorf("failed to store the recursive proof, %v", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	// NOTE(pg): the defer func is useless from now on, use a different variable
	// name for errors (or shadow err in inner scopes) to not trigger it.

	// state is up to date, check if we can send the final proof using the
	// one just crafted.
	finalProofBuilt, finalProofErr := a.tryBuildFinalProof(ctx, prover, proof)
	if finalProofErr != nil {
		// just log the error and continue to handle the aggregated proof
		log.Errorf("Failed trying to check if recursive proof can be verified: %v", finalProofErr)
	}

	// NOTE(pg): prover is done, use a.ctx from now on

	if !finalProofBuilt {
		proof.GeneratingSince = nil

		// final proof has not been generated, update the recursive proof
		err := a.State.UpdateGeneratedProof(a.ctx, proof, nil)
		if err != nil {
			err = fmt.Errorf("failed to store batch proof result, %v", err)
			log.Error(FirstToUpper(err.Error()))
			return false, err
		}
	}

	return true, nil
}

func (a *Aggregator) getAndLockBatchToProve(ctx context.Context, prover proverInterface) (*state.Batch, *state.Proof, error) {
	proverID := prover.ID()
	proverName := prover.Name()

	log := log.WithFields(
		"prover", proverName,
		"proverId", proverID,
		"proverAddr", prover.Addr(),
	)

	a.StateDBMutex.Lock()
	defer a.StateDBMutex.Unlock()

	lastVerifiedBatch, err := a.State.GetLastVerifiedBatch(ctx, nil)
	if err != nil {
		return nil, nil, err
	}

	// Get virtual batch pending to generate proof
	batchToVerify, err := a.State.GetVirtualBatchToProve(ctx, lastVerifiedBatch.BatchNumber, nil)
	if err != nil {
		return nil, nil, err
	}

	log.Infof("Found virtual batch %d pending to generate proof", batchToVerify.BatchNumber)
	log = log.WithFields("batch", batchToVerify.BatchNumber)

	log.Info("Checking profitability to aggregate batch")

	// pass matic collateral as zero here, bcs in smart contract fee for aggregator is not defined yet
	isProfitable, err := a.ProfitabilityChecker.IsProfitable(ctx, big.NewInt(0))
	if err != nil {
		log.Errorf("Failed to check aggregator profitability, err: %v", err)
		return nil, nil, err
	}

	if !isProfitable {
		log.Infof("Batch is not profitable, matic collateral %d", big.NewInt(0))
		return nil, nil, err
	}

	now := time.Now().Round(time.Microsecond)
	proof := &state.Proof{
		BatchNumber:      batchToVerify.BatchNumber,
		BatchNumberFinal: batchToVerify.BatchNumber,
		Prover:           &proverName,
		ProverID:         &proverID,
		GeneratingSince:  &now,
	}

	// Avoid other prover to process the same batch
	err = a.State.AddGeneratedProof(ctx, proof, nil)
	if err != nil {
		log.Errorf("Failed to add batch proof, err: %v", err)
		return nil, nil, err
	}

	return batchToVerify, proof, nil
}

func (a *Aggregator) tryGenerateBatchProof(ctx context.Context, prover proverInterface) (bool, error) {
	log := log.WithFields(
		"prover", prover.Name(),
		"proverId", prover.ID(),
		"proverAddr", prover.Addr(),
	)
	log.Debug("tryGenerateBatchProof start")

	batchToProve, proof, err0 := a.getAndLockBatchToProve(ctx, prover)
	if errors.Is(err0, state.ErrNotFound) {
		// nothing to proof, swallow the error
		log.Debug("Nothing to generate proof")
		return false, nil
	}
	if err0 != nil {
		return false, err0
	}

	log = log.WithFields("batch", batchToProve.BatchNumber)

	var (
		genProofID *string
		err        error
	)

	defer func() {
		if err != nil {
			err2 := a.State.DeleteGeneratedProofs(a.ctx, proof.BatchNumber, proof.BatchNumberFinal, nil)
			if err2 != nil {
				log.Errorf("Failed to delete proof in progress, err: %v", err2)
			}
		}
		log.Debug("tryGenerateBatchProof end")
	}()

	log.Info("Generating proof from batch")

	log.Infof("Sending zki + batch to the prover, batchNumber [%d]", batchToProve.BatchNumber)
	inputProver, err := a.buildInputProver(ctx, batchToProve)
	if err != nil {
		err = fmt.Errorf("failed to build input prover, %v", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	b, err := json.Marshal(inputProver)
	if err != nil {
		err = fmt.Errorf("failed to serialize input prover, %v", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	proof.InputProver = string(b)

	log.Infof("Sending a batch to the prover. OldStateRoot [%#x], OldBatchNum [%d]",
		inputProver.PublicInputs.OldStateRoot, inputProver.PublicInputs.OldBatchNum)

	genProofID, err = prover.BatchProof(inputProver)
	if err != nil {
		err = fmt.Errorf("failed to get batch proof id, %v", err)
		log.Error(FirstToUpper(err.Error()))
		return false, err
	}

	proof.ProofID = genProofID

	log.Infof("Proof ID %v", *proof.ProofID)
	log = log.WithFields("proofId", *proof.ProofID)

	monitoredTxID := fmt.Sprintf(monitoredHashIDFormat, proof.BatchNumber, proof.BatchNumberFinal)
	_, errFinalProof := a.State.GetFinalProofByMonitoredId(a.ctx, monitoredTxID, nil)
	if errFinalProof != nil && errFinalProof != state.ErrNotFound {
		log.Errorf("failed to read finalProof from table. err: %v", err)
		return false, errFinalProof
	}

	if errFinalProof == state.ErrNotFound {
		resGetProof, err := prover.WaitRecursiveProof(ctx, *proof.ProofID)
		if err != nil {
			err = fmt.Errorf("failed to get proof from prover, %v", err)
			log.Error(FirstToUpper(err.Error()))
			return false, err
		}

		log.Info("Batch proof generated")

		proof.Proof = resGetProof
	}

	// NOTE(pg): the defer func is useless from now on, use a different variable
	// name for errors (or shadow err in inner scopes) to not trigger it.

	finalProofBuilt, finalProofErr := a.tryBuildFinalProof(ctx, prover, proof)
	if finalProofErr != nil {
		// just log the error and continue to handle the generated proof
		log.Errorf("Error trying to build final proof: %v", finalProofErr)
	}

	// NOTE(pg): prover is done, use a.ctx from now on

	if !finalProofBuilt {
		proof.GeneratingSince = nil

		// final proof has not been generated, update the batch proof
		err := a.State.UpdateGeneratedProof(a.ctx, proof, nil)
		if err != nil {
			err = fmt.Errorf("failed to store batch proof result, %v", err)
			log.Error(FirstToUpper(err.Error()))
			return false, err
		}
	}

	return true, nil
}

// canVerifyProof returns true if we have reached the timeout to verify a proof
// and no other prover is verifying a proof (verifyingProof = false).
func (a *Aggregator) canVerifyProof() bool {
	a.TimeSendFinalProofMutex.RLock()
	defer a.TimeSendFinalProofMutex.RUnlock()
	return a.TimeSendFinalProof.Before(time.Now()) && !a.verifyingProof
}

// startProofVerification sets to true the verifyingProof variable to indicate that there is a proof verification in progress
func (a *Aggregator) startProofVerification() {
	a.TimeSendFinalProofMutex.Lock()
	defer a.TimeSendFinalProofMutex.Unlock()
	a.verifyingProof = true
}

// endProofVerification set verifyingProof to false to indicate that there is not proof verification in progress
func (a *Aggregator) endProofVerification() {
	a.TimeSendFinalProofMutex.Lock()
	defer a.TimeSendFinalProofMutex.Unlock()
	a.verifyingProof = false
}

// resetVerifyProofTime updates the timeout to verify a proof.
func (a *Aggregator) resetVerifyProofTime() {
	a.TimeSendFinalProofMutex.Lock()
	defer a.TimeSendFinalProofMutex.Unlock()
	a.TimeSendFinalProof = time.Now().Add(a.cfg.VerifyProofInterval.Duration)
}

func (a *Aggregator) canVerifyProofHash() bool {
	a.TimeSendFinalProofHashMutex.RLock()
	defer a.TimeSendFinalProofHashMutex.RUnlock()
	return a.TimeSendFinalProofHash.Before(time.Now()) && !a.verifyingProofHash
}

func (a *Aggregator) startProofHash() {
	a.TimeSendFinalProofHashMutex.Lock()
	defer a.TimeSendFinalProofHashMutex.Unlock()
	a.verifyingProofHash = true
}

// endProofVerification set verifyingProof to false to indicate that there is not proof verification in progress
func (a *Aggregator) endProofHash() {
	a.TimeSendFinalProofHashMutex.Lock()
	defer a.TimeSendFinalProofHashMutex.Unlock()
	a.verifyingProofHash = false
}

// resetVerifyProofTime updates the timeout to verify a proof.
func (a *Aggregator) resetVerifyProofHashTime() {
	a.TimeSendFinalProofHashMutex.Lock()
	defer a.TimeSendFinalProofHashMutex.Unlock()
	a.TimeSendFinalProofHash = time.Now().Add(a.cfg.VerifyProofInterval.Duration)
}

// isSynced checks if the state is synchronized with L1. If a batch number is
// provided, it makes sure that the state is synced with that batch.
func (a *Aggregator) isSynced(ctx context.Context, batchNum *uint64) bool {
	// get latest verified batch as seen by the synchronizer
	lastVerifiedBatch, err := a.State.GetLastVerifiedBatch(ctx, nil)
	if err == state.ErrNotFound {
		return false
	}
	if err != nil {
		log.Warnf("Failed to get last consolidated batch: %v", err)
		return false
	}

	if lastVerifiedBatch == nil {
		return false
	}

	if batchNum != nil && lastVerifiedBatch.BatchNumber < *batchNum {
		log.Infof("Waiting for the state to be synced, lastVerifiedBatchNum: %d, waiting for batch: %d", lastVerifiedBatch.BatchNumber, *batchNum)
		return false
	}

	// latest verified batch in L1
	lastVerifiedEthBatchNum, err := a.Ethman.GetLatestVerifiedBatchNum()
	if err != nil {
		log.Warnf("Failed to get last eth batch, err: %v", err)
		return false
	}

	// check if L2 is synced with L1
	if lastVerifiedBatch.BatchNumber < lastVerifiedEthBatchNum {
		log.Infof("Waiting for the state to be synced, lastVerifiedBatchNum: %d, lastVerifiedEthBatchNum: %d, waiting for batch",
			lastVerifiedBatch.BatchNumber, lastVerifiedEthBatchNum)
		return false
	}

	return true
}

func (a *Aggregator) buildInputProver(ctx context.Context, batchToVerify *state.Batch) (*pb.InputProver, error) {
	previousBatch, err := a.State.GetBatchByNumber(ctx, batchToVerify.BatchNumber-1, nil)
	if err != nil && err != state.ErrStateNotSynchronized {
		return nil, fmt.Errorf("failed to get previous batch, err: %v", err)
	}

	inputProver := &pb.InputProver{
		PublicInputs: &pb.PublicInputs{
			OldStateRoot:    previousBatch.StateRoot.Bytes(),
			OldAccInputHash: previousBatch.AccInputHash.Bytes(),
			OldBatchNum:     previousBatch.BatchNumber,
			ChainId:         a.cfg.ChainID,
			ForkId:          a.cfg.ForkId,
			BatchL2Data:     batchToVerify.BatchL2Data,
			GlobalExitRoot:  batchToVerify.GlobalExitRoot.Bytes(),
			EthTimestamp:    uint64(batchToVerify.Timestamp.Unix()),
			SequencerAddr:   batchToVerify.Coinbase.String(),
			AggregatorAddr:  a.cfg.SenderAddress,
		},
		Db:                map[string]string{},
		ContractsBytecode: map[string]string{},
	}

	return inputProver, nil
}

// healthChecker will provide an implementation of the HealthCheck interface.
type healthChecker struct{}

// newHealthChecker returns a health checker according to standard package
// grpc.health.v1.
func newHealthChecker() *healthChecker {
	return &healthChecker{}
}

// HealthCheck interface implementation.

// Check returns the current status of the server for unary gRPC health requests,
// for now if the server is up and able to respond we will always return SERVING.
func (hc *healthChecker) Check(ctx context.Context, req *grpchealth.HealthCheckRequest) (*grpchealth.HealthCheckResponse, error) {
	log.Info("Serving the Check request for health check")
	return &grpchealth.HealthCheckResponse{
		Status: grpchealth.HealthCheckResponse_SERVING,
	}, nil
}

// Watch returns the current status of the server for stream gRPC health requests,
// for now if the server is up and able to respond we will always return SERVING.
func (hc *healthChecker) Watch(req *grpchealth.HealthCheckRequest, server grpchealth.Health_WatchServer) error {
	log.Info("Serving the Watch request for health check")
	return server.Send(&grpchealth.HealthCheckResponse{
		Status: grpchealth.HealthCheckResponse_SERVING,
	})
}

func (a *Aggregator) handleMonitoredTxResult(result ethtxmanager.MonitoredTxResult) {
	resLog := log.WithFields("owner", ethTxManagerOwner, "txId", result.ID)
	if result.Status == ethtxmanager.MonitoredTxStatusFailed {
		resLog.Error("failed to send batch verification, TODO: review this fatal and define what to do in this case")
		if err := a.EthTxManager.UpdateId(a.ctx, result.ID, nil); err != nil {
			resLog.Error(err)
		}
		return
	}

	if strings.Contains(result.ID, "proof-hash-from-") {
		return
	}

	// monitoredIDFormat: "proof-from-%v-to-%v"
	idSlice := strings.Split(result.ID, "-")
	if len(idSlice) == 6 {
		return
	}
	proofBatchNumberStr := idSlice[2]

	proofBatchNumber, err := strconv.ParseUint(proofBatchNumberStr, encoding.Base10, 0)
	if err != nil {
		resLog.Errorf("failed to read final proof batch number from monitored tx: %v", err)
	}

	proofBatchNumberFinalStr := idSlice[4]
	proofBatchNumberFinal, err := strconv.ParseUint(proofBatchNumberFinalStr, encoding.Base10, 0)
	if err != nil {
		resLog.Errorf("failed to read final proof batch number final from monitored tx: %v", err)
	}

	log := log.WithFields("txId", result.ID, "batches", fmt.Sprintf("%d-%d", proofBatchNumber, proofBatchNumberFinal))
	log.Info("Final proof verified")

	// wait for the synchronizer to catch up the verified batches
	log.Debug("A final proof has been sent, waiting for the network to be synced")
	for !a.isSynced(a.ctx, &proofBatchNumberFinal) {
		log.Info("Waiting for synchronizer to sync...")
		time.Sleep(a.cfg.RetryTime.Duration)
	}

	// network is synced with the final proof, we can safely delete all recursive
	// proofs up to the last synced batch
	err = a.State.CleanupGeneratedProofs(a.ctx, proofBatchNumberFinal, nil)
	if err != nil {
		log.Errorf("Failed to store proof aggregation result: %v", err)
	}
}

func buildMonitoredTxID(batchNumber, batchNumberFinal uint64) string {
	return fmt.Sprintf(monitoredIDFormat, batchNumber, batchNumberFinal)
}

func (a *Aggregator) cleanupLockedProofs() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(a.TimeCleanupLockedProofs.Duration):
			n, err := a.State.CleanupLockedProofs(a.ctx, a.cfg.GeneratingProofCleanupThreshold, nil)
			if err != nil {
				log.Errorf("Failed to cleanup locked proofs: %v", err)
			}
			if n == 1 {
				log.Warn("Found a stale proof and removed form cache")
			} else if n > 1 {
				log.Warnf("Found %d stale proofs and removed from cache", n)
			}
		}
	}
}

// FirstToUpper returns the string passed as argument with the first letter in
// uppercase.
func FirstToUpper(s string) string {
	runes := []rune(s)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

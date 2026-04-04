package services

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"

	"bountyvault/internal/config"

	"github.com/algorand/go-algorand-sdk/v2/client/v2/algod"
	"github.com/algorand/go-algorand-sdk/v2/client/v2/indexer"
	"github.com/algorand/go-algorand-sdk/v2/crypto"
	"github.com/algorand/go-algorand-sdk/v2/encoding/msgpack"
	"github.com/algorand/go-algorand-sdk/v2/transaction"
	"github.com/algorand/go-algorand-sdk/v2/types"
)

// AlgorandService handles blockchain interactions
type AlgorandService struct {
	algodClient   *algod.Client
	indexerClient *indexer.Client
	appID         uint64
	network       string
}

// NewAlgorandService creates a new Algorand service instance
func NewAlgorandService(cfg *config.Config) (*AlgorandService, error) {
	algodClient, err := algod.MakeClient(cfg.AlgoNodeURL, "")
	if err != nil {
		return nil, fmt.Errorf("failed to create algod client: %w", err)
	}

	indexerClient, err := indexer.MakeClient(cfg.AlgoIndexerURL, "")
	if err != nil {
		return nil, fmt.Errorf("failed to create indexer client: %w", err)
	}

	return &AlgorandService{
		algodClient:   algodClient,
		indexerClient: indexerClient,
		appID:         cfg.BountyAppID,
		network:       cfg.AlgoNetwork,
	}, nil
}

// GetSuggestedParams fetches current network parameters for transactions
func (s *AlgorandService) GetSuggestedParams(ctx context.Context) (types.SuggestedParams, error) {
	params, err := s.algodClient.SuggestedParams().Do(ctx)
	if err != nil {
		return types.SuggestedParams{}, fmt.Errorf("failed to get suggested params: %w", err)
	}
	return params, nil
}

// UnsignedTxnResult holds encoded unsigned transactions for client-side signing
type UnsignedTxnResult struct {
	Transactions []string `json:"transactions"` // base64 encoded unsigned transactions
	GroupID      string   `json:"group_id"`      // base64 encoded group ID
	Message      string   `json:"message"`
}

// BuildCreateBountyTxns builds unsigned transactions for creating a bounty
// Returns base64-encoded transactions for the frontend to sign with wallet
func (s *AlgorandService) BuildCreateBountyTxns(
	ctx context.Context,
	creatorAddr string,
	rewardMicroAlgos uint64,
	termsHash []byte,
	deadline uint64,
	maxSubmissions uint64,
	arbitratorAddr string,
) (*UnsignedTxnResult, error) {
	params, err := s.GetSuggestedParams(ctx)
	if err != nil {
		return nil, err
	}

	appAddr := crypto.GetApplicationAddress(s.appID)

	// Transaction 1: Payment to escrow
	payTxn, err := transaction.MakePaymentTxn(
		creatorAddr,
		appAddr.String(),
		rewardMicroAlgos,
		nil,
		"",
		params,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create payment txn: %w", err)
	}

	// Transaction 2: Application call (create_bounty ABI method)
	// ABI method selector for create_bounty
	methodSelector := []byte{0x00, 0x00, 0x00, 0x00} // Placeholder — computed from ABI JSON
	// Ensure termsHash is never nil — Algorand SDK cannot encode nil byte arrays
	if termsHash == nil {
		termsHash = []byte{}
	}

	appArgs := [][]byte{
		methodSelector,
		termsHash,
		uint64ToBytes(deadline),
		uint64ToBytes(maxSubmissions),
	}

	var foreignAccounts []string
	if arbitratorAddr != "" {
		foreignAccounts = []string{arbitratorAddr}
	}

	senderAddr, err := types.DecodeAddress(creatorAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid creator address: %w", err)
	}

	appCallTxn, err := transaction.MakeApplicationNoOpTx(
		s.appID,
		appArgs,
		foreignAccounts,
		nil, nil,
		params,
		senderAddr, nil, types.Digest{}, [32]byte{}, types.Address{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create app call txn: %w", err)
	}

	// Group transactions
	gid, err := crypto.ComputeGroupID([]types.Transaction{payTxn, appCallTxn})
	if err != nil {
		return nil, fmt.Errorf("failed to compute group ID: %w", err)
	}
	payTxn.Group = gid
	appCallTxn.Group = gid

	// Encode transactions
	payTxnBytes := encodeMsgpack(payTxn)
	appCallTxnBytes := encodeMsgpack(appCallTxn)

	return &UnsignedTxnResult{
		Transactions: []string{
			base64.StdEncoding.EncodeToString(payTxnBytes),
			base64.StdEncoding.EncodeToString(appCallTxnBytes),
		},
		GroupID: base64.StdEncoding.EncodeToString(gid[:]),
		Message: "Sign both transactions with your wallet",
	}, nil
}

// BuildSubmitProofTxn builds an unsigned transaction for submitting work proof
func (s *AlgorandService) BuildSubmitProofTxn(
	ctx context.Context,
	workerAddr string,
	workHash []byte,
) (*UnsignedTxnResult, error) {
	params, err := s.GetSuggestedParams(ctx)
	if err != nil {
		return nil, err
	}

	methodSelector := []byte{0x00, 0x00, 0x00, 0x01} // Placeholder
	appArgs := [][]byte{
		methodSelector,
		workHash,
	}

	senderAddr, err := types.DecodeAddress(workerAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid worker address: %w", err)
	}

	appCallTxn, err := transaction.MakeApplicationNoOpTx(
		s.appID,
		appArgs,
		nil, nil, nil,
		params,
		senderAddr, nil, types.Digest{}, [32]byte{}, types.Address{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create submit proof txn: %w", err)
	}

	txnBytes := encodeMsgpack(appCallTxn)

	return &UnsignedTxnResult{
		Transactions: []string{
			base64.StdEncoding.EncodeToString(txnBytes),
		},
		Message: "Sign this transaction to submit your work proof",
	}, nil
}

// BuildApprovePayoutTxn builds an unsigned transaction for approving a worker's payout
func (s *AlgorandService) BuildApprovePayoutTxn(
	ctx context.Context,
	creatorAddr string,
	workerAddr string,
) (*UnsignedTxnResult, error) {
	params, err := s.GetSuggestedParams(ctx)
	if err != nil {
		return nil, err
	}

	methodSelector := []byte{0x00, 0x00, 0x00, 0x02} // Placeholder
	appArgs := [][]byte{methodSelector}
	foreignAccounts := []string{workerAddr}

	senderAddr, err := types.DecodeAddress(creatorAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid creator address: %w", err)
	}

	appCallTxn, err := transaction.MakeApplicationNoOpTx(
		s.appID,
		appArgs,
		foreignAccounts,
		nil, nil,
		params,
		senderAddr, nil, types.Digest{}, [32]byte{}, types.Address{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create approve payout txn: %w", err)
	}

	txnBytes := encodeMsgpack(appCallTxn)

	return &UnsignedTxnResult{
		Transactions: []string{
			base64.StdEncoding.EncodeToString(txnBytes),
		},
		Message: "Sign this transaction to approve the worker's payout",
	}, nil
}

// BuildDisputeTxn builds an unsigned transaction for initiating a dispute
func (s *AlgorandService) BuildDisputeTxn(
	ctx context.Context,
	senderAddr string,
	evidenceHash []byte,
) (*UnsignedTxnResult, error) {
	params, err := s.GetSuggestedParams(ctx)
	if err != nil {
		return nil, err
	}

	methodSelector := []byte{0x00, 0x00, 0x00, 0x03} // Placeholder
	appArgs := [][]byte{
		methodSelector,
		evidenceHash,
	}

	senderAddress, err := types.DecodeAddress(senderAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid sender address: %w", err)
	}

	appCallTxn, err := transaction.MakeApplicationNoOpTx(
		s.appID,
		appArgs,
		nil, nil, nil,
		params,
		senderAddress, nil, types.Digest{}, [32]byte{}, types.Address{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create dispute txn: %w", err)
	}

	txnBytes := encodeMsgpack(appCallTxn)

	return &UnsignedTxnResult{
		Transactions: []string{
			base64.StdEncoding.EncodeToString(txnBytes),
		},
		Message: "Sign this transaction to initiate a dispute",
	}, nil
}

// BuildDAOVoteTxn builds an unsigned transaction for DAO voting
func (s *AlgorandService) BuildDAOVoteTxn(
	ctx context.Context,
	voterAddr string,
	support uint64, // 1=approve, 2=reject
) (*UnsignedTxnResult, error) {
	params, err := s.GetSuggestedParams(ctx)
	if err != nil {
		return nil, err
	}

	methodSelector := []byte{0x00, 0x00, 0x00, 0x04} // Placeholder
	appArgs := [][]byte{
		methodSelector,
		uint64ToBytes(support),
	}

	senderAddr, err := types.DecodeAddress(voterAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid voter address: %w", err)
	}

	appCallTxn, err := transaction.MakeApplicationNoOpTx(
		s.appID,
		appArgs,
		nil, nil, nil,
		params,
		senderAddr, nil, types.Digest{}, [32]byte{}, types.Address{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create DAO vote txn: %w", err)
	}

	txnBytes := encodeMsgpack(appCallTxn)

	return &UnsignedTxnResult{
		Transactions: []string{
			base64.StdEncoding.EncodeToString(txnBytes),
		},
		Message: "Sign this transaction to cast your DAO vote",
	}, nil
}

// SubmitSignedTxns submits signed transactions to the Algorand network
func (s *AlgorandService) SubmitSignedTxns(ctx context.Context, signedTxnsB64 []string) (string, error) {
	var allTxnBytes []byte
	for _, b64 := range signedTxnsB64 {
		txnBytes, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return "", fmt.Errorf("failed to decode signed txn: %w", err)
		}
		allTxnBytes = append(allTxnBytes, txnBytes...)
	}

	txID, err := s.algodClient.SendRawTransaction(allTxnBytes).Do(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to submit transaction: %w", err)
	}

	return txID, nil
}

// WaitForConfirmation waits for a transaction to be confirmed
func (s *AlgorandService) WaitForConfirmation(ctx context.Context, txID string, rounds uint64) error {
	status, err := s.algodClient.Status().Do(ctx)
	if err != nil {
		return fmt.Errorf("failed to get status: %w", err)
	}

	lastRound := status.LastRound
	for i := uint64(0); i < rounds; i++ {
		info, _, err := s.algodClient.PendingTransactionInformation(txID).Do(ctx)
		if err != nil {
			return fmt.Errorf("failed to get pending txn info: %w", err)
		}

		if info.ConfirmedRound > 0 {
			log.Printf("Transaction %s confirmed in round %d", txID, info.ConfirmedRound)
			return nil
		}

		// Wait for next round
		s.algodClient.StatusAfterBlock(lastRound + 1).Do(ctx)
		lastRound++
	}

	return fmt.Errorf("transaction %s not confirmed after %d rounds", txID, rounds)
}

// GetApplicationState reads the global state of the bounty escrow contract
func (s *AlgorandService) GetApplicationState(ctx context.Context) (map[string]interface{}, error) {
	app, err := s.algodClient.GetApplicationByID(s.appID).Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get application: %w", err)
	}

	state := make(map[string]interface{})
	for _, kv := range app.Params.GlobalState {
		keyBytes, _ := base64.StdEncoding.DecodeString(kv.Key)
		key := string(keyBytes)

		if kv.Value.Type == 1 { // bytes
			state[key] = kv.Value.Bytes
		} else { // uint
			state[key] = kv.Value.Uint
		}
	}

	return state, nil
}

// GetAccountBalance returns the ALGO balance of an address
func (s *AlgorandService) GetAccountBalance(ctx context.Context, address string) (uint64, error) {
	info, err := s.algodClient.AccountInformation(address).Do(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get account info: %w", err)
	}
	return info.Amount, nil
}

// Helper: encode transaction to msgpack bytes
func encodeMsgpack(txn types.Transaction) []byte {
	return msgpack.Encode(txn)
}

// Helper: convert uint64 to big-endian bytes
func uint64ToBytes(val uint64) []byte {
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = byte(val & 0xff)
		val >>= 8
	}
	return b
}

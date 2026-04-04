package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"bountyvault/internal/config"
	"bountyvault/internal/database"
	"bountyvault/internal/models"
	"bountyvault/internal/services"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// BountyHandler handles bounty CRUD and workflow endpoints (v3.1)
type BountyHandler struct {
	cfg     *config.Config
	algoSvc *services.AlgorandService
	ipfsSvc *services.IPFSService
	r2Svc   *services.R2Service
}

func NewBountyHandler(cfg *config.Config, algoSvc *services.AlgorandService, ipfsSvc *services.IPFSService, r2Svc *services.R2Service) *BountyHandler {
	return &BountyHandler{cfg: cfg, algoSvc: algoSvc, ipfsSvc: ipfsSvc, r2Svc: r2Svc}
}

// GET /api/bounties — Public listing with filters
func (h *BountyHandler) ListBounties(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "12"))
	status := c.Query("status")
	search := c.Query("search")
	sortBy := c.DefaultQuery("sort", "created_at")
	order := c.DefaultQuery("order", "desc")
	tags := c.Query("tags")

	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 50 {
		pageSize = 12
	}

	where := []string{"1=1"}
	args := []interface{}{}
	idx := 1

	if status != "" {
		where = append(where, fmt.Sprintf("b.status = $%d", idx))
		args = append(args, status)
		idx++
	}
	if search != "" {
		where = append(where, fmt.Sprintf("(b.title ILIKE $%d OR b.description ILIKE $%d)", idx, idx))
		args = append(args, "%"+search+"%")
		idx++
	}
	if tags != "" {
		where = append(where, fmt.Sprintf("b.tags && $%d", idx))
		args = append(args, pq.Array(strings.Split(tags, ",")))
		idx++
	}

	wc := strings.Join(where, " AND ")
	validSorts := map[string]string{
		"created_at": "b.created_at",
		"reward":     "b.reward_algo",
		"deadline":   "b.deadline",
	}
	sortCol := validSorts[sortBy]
	if sortCol == "" {
		sortCol = "b.created_at"
	}
	if order != "asc" {
		order = "desc"
	}

	var total int
	database.DB.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM bounties b WHERE %s`, wc), args...).Scan(&total)

	offset := (page - 1) * pageSize
	q := fmt.Sprintf(`
		SELECT b.id, b.bounty_id, b.creator_id, b.title, b.description, b.reward_algo,
		       b.deadline, b.status, b.app_id, b.max_submissions, b.submissions_remaining,
		       b.tags, b.created_at, b.updated_at,
		       p.id, p.username, p.display_name, p.avatar_url, p.reputation_score,
		       (SELECT COUNT(*) FROM submissions s WHERE s.bounty_id = b.id)
		FROM bounties b JOIN profiles p ON b.creator_id = p.id
		WHERE %s ORDER BY %s %s LIMIT $%d OFFSET $%d
	`, wc, sortCol, order, idx, idx+1)
	args = append(args, pageSize, offset)

	rows, err := database.DB.Query(q, args...)
	if err != nil {
		c.JSON(500, models.APIResponse{Success: false, Error: "Failed to fetch bounties"})
		return
	}
	defer rows.Close()

	bounties := []models.Bounty{}
	for rows.Next() {
		var b models.Bounty
		var cr models.Profile
		var sc int
		rows.Scan(
			&b.ID, &b.BountyID, &b.CreatorID, &b.Title, &b.Description, &b.RewardAlgo,
			&b.Deadline, &b.Status, &b.AppID, &b.MaxSubmissions, &b.SubmissionsRemaining,
			pq.Array(&b.Tags), &b.CreatedAt, &b.UpdatedAt,
			&cr.ID, &cr.Username, &cr.DisplayName, &cr.AvatarURL, &cr.ReputationScore, &sc,
		)
		b.Creator = &cr
		b.SubmissionCount = sc
		bounties = append(bounties, b)
	}

	c.JSON(200, models.APIResponse{Success: true, Data: models.PaginatedResponse{
		Items: bounties, TotalCount: total, Page: page, PageSize: pageSize,
		TotalPages: int(math.Ceil(float64(total) / float64(pageSize))),
	}})
}

// GET /api/bounties/:id — Get single bounty by UUID or bounty_id (CR00847)
func (h *BountyHandler) GetBounty(c *gin.Context) {
	id := c.Param("id")
	var b models.Bounty
	var cr models.Profile
	var sc int

	// Support lookup by both UUID and bounty_id (CR format)
	whereClause := "b.id = $1"
	if strings.HasPrefix(strings.ToUpper(id), "CR") {
		whereClause = "b.bounty_id = $1"
	}

	err := database.DB.QueryRow(fmt.Sprintf(`
		SELECT b.id, b.bounty_id, b.creator_id, b.title, b.description, b.reward_algo,
		       b.deadline, b.status, b.app_id, b.escrow_txn_id, b.payout_txn_id,
		       b.max_submissions, b.submissions_remaining, b.tags, b.created_at, b.updated_at,
		       p.id, p.username, p.display_name, p.avatar_url, p.reputation_score,
		       (SELECT COUNT(*) FROM submissions s WHERE s.bounty_id = b.id)
		FROM bounties b JOIN profiles p ON b.creator_id = p.id WHERE %s
	`, whereClause), id).Scan(
		&b.ID, &b.BountyID, &b.CreatorID, &b.Title, &b.Description, &b.RewardAlgo,
		&b.Deadline, &b.Status, &b.AppID, &b.EscrowTxnID, &b.PayoutTxnID,
		&b.MaxSubmissions, &b.SubmissionsRemaining, pq.Array(&b.Tags), &b.CreatedAt, &b.UpdatedAt,
		&cr.ID, &cr.Username, &cr.DisplayName, &cr.AvatarURL, &cr.ReputationScore, &sc,
	)

	if err == sql.ErrNoRows {
		c.JSON(404, models.APIResponse{Success: false, Error: "Bounty not found"})
		return
	}
	if err != nil {
		c.JSON(500, models.APIResponse{Success: false, Error: "Failed to fetch bounty"})
		return
	}
	b.Creator = &cr
	b.SubmissionCount = sc
	c.JSON(200, models.APIResponse{Success: true, Data: b})
}

// POST /api/bounties — Create bounty in DB (no IPFS yet — happens on confirm-lock)
func (h *BountyHandler) CreateBounty(c *gin.Context) {
	pid, _ := c.Get("profile_id")
	var req models.CreateBountyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// Translate validation errors into user-friendly messages
		errMsg := err.Error()
		if strings.Contains(errMsg, "'Title'") && strings.Contains(errMsg, "'min'") {
			errMsg = "Title must be at least 5 characters long"
		} else if strings.Contains(errMsg, "'Description'") && strings.Contains(errMsg, "'min'") {
			errMsg = "Description must be at least 20 characters long"
		} else if strings.Contains(errMsg, "'Title'") && strings.Contains(errMsg, "'required'") {
			errMsg = "Title is required"
		} else if strings.Contains(errMsg, "'Description'") && strings.Contains(errMsg, "'required'") {
			errMsg = "Description is required"
		}
		// Handle multiple validation errors
		if strings.Contains(err.Error(), "'Title'") && strings.Contains(err.Error(), "'Description'") {
			errMsg = "Title must be at least 5 characters and Description must be at least 20 characters"
		}
		c.JSON(400, models.APIResponse{Success: false, Error: errMsg})
		return
	}

	deadline, err := time.Parse(time.RFC3339, req.Deadline)
	if err != nil {
		c.JSON(400, models.APIResponse{Success: false, Error: "Invalid deadline format (use RFC3339)"})
		return
	}
	if deadline.Before(time.Now().Add(1 * time.Hour)) {
		c.JSON(400, models.APIResponse{Success: false, Error: "Deadline must be at least 1 hour in the future"})
		return
	}
	if req.RewardAlgo < 1.0 {
		c.JSON(400, models.APIResponse{Success: false, Error: "Minimum reward is 1 ALGO"})
		return
	}

	bid := uuid.New()
	// Generate unique CR-format bounty ID from DB function
	var bountyID string
	err = database.DB.QueryRow(`SELECT generate_bounty_id()`).Scan(&bountyID)
	if err != nil {
		c.JSON(500, models.APIResponse{Success: false, Error: "Failed to generate bounty ID"})
		return
	}

	_, err = database.DB.Exec(`
		INSERT INTO bounties (id, bounty_id, creator_id, title, description, reward_algo,
		  deadline, max_submissions, submissions_remaining, tags, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8, $9, 'open')`,
		bid, bountyID, pid, req.Title, req.Description, req.RewardAlgo,
		deadline, req.MaxSubmissions, pq.Array(req.Tags))
	if err != nil {
		c.JSON(500, models.APIResponse{Success: false, Error: "Failed to create bounty: " + err.Error()})
		return
	}

	database.DB.Exec(`UPDATE profiles SET total_bounties_created = total_bounties_created + 1 WHERE id = $1`, pid)

	c.JSON(201, models.APIResponse{
		Success: true,
		Message: "Bounty created — now lock funds to publish",
		Data:    gin.H{"id": bid, "bounty_id": bountyID},
	})
}

// POST /api/bounties/:id/lock — Build unsigned transactions for Pera Wallet
func (h *BountyHandler) LockBounty(c *gin.Context) {
	bid := c.Param("id")
	pid, _ := c.Get("profile_id")
	var req models.LockBountyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Error: err.Error()})
		return
	}

	var creatorID string
	var ra float64
	var dl time.Time
	var ms int
	err := database.DB.QueryRow(`
		SELECT creator_id, reward_algo, deadline, max_submissions
		FROM bounties WHERE id = $1 AND status = 'open'
	`, bid).Scan(&creatorID, &ra, &dl, &ms)
	if err != nil {
		c.JSON(404, models.APIResponse{Success: false, Error: "Bounty not found or not in open state"})
		return
	}
	if creatorID != pid.(string) {
		c.JSON(403, models.APIResponse{Success: false, Error: "Only the creator can lock funds"})
		return
	}

	txns, err := h.algoSvc.BuildCreateBountyTxns(
		c.Request.Context(), req.WalletAddress,
		uint64(ra*1e6), nil, uint64(dl.Unix()), uint64(ms), "",
	)
	if err != nil {
		c.JSON(500, models.APIResponse{Success: false, Error: err.Error()})
		return
	}

	c.JSON(200, models.APIResponse{Success: true, Message: "Sign both transactions with Pera Wallet", Data: txns})
}

// POST /api/bounties/:id/confirm-lock — Submit signed txns, pin to IPFS, record
func (h *BountyHandler) ConfirmLock(c *gin.Context) {
	bid := c.Param("id")
	pid, _ := c.Get("profile_id")
	var req models.ConfirmLockRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Error: err.Error()})
		return
	}

	txID, err := h.algoSvc.SubmitSignedTxns(c.Request.Context(), req.SignedTxns)
	if err != nil {
		c.JSON(500, models.APIResponse{Success: false, Error: "Transaction submission failed: " + err.Error()})
		return
	}
	h.algoSvc.WaitForConfirmation(c.Request.Context(), txID, 10)

	// Fetch bounty details for IPFS metadata
	var b models.Bounty
	database.DB.QueryRow(`SELECT bounty_id, reward_algo, max_submissions, deadline, tags FROM bounties WHERE id = $1`, bid).Scan(
		&b.BountyID, &b.RewardAlgo, &b.MaxSubmissions, &b.Deadline, pq.Array(&b.Tags),
	)

	// Pin bounty creation metadata to IPFS (fire-and-forget)
	var ipfsCID, ipfsURL string
	go func() {
		var walletAddr string
		database.DB.QueryRow(`SELECT COALESCE(wallet_address,'') FROM profiles WHERE id = $1`, pid).Scan(&walletAddr)
		result, err := h.ipfsSvc.PinBountyCreated(context.Background(), services.BountyCreatedMetadata{
			BountyID:       b.BountyID,
			AppID:          req.AppID,
			CreatorAddress: walletAddr,
			RewardAlgo:     b.RewardAlgo,
			MaxSubmissions: b.MaxSubmissions,
			Deadline:       b.Deadline.Format(time.RFC3339),
			Tags:           b.Tags,
			TxnID:          txID,
			Network:        h.cfg.AlgoNetwork,
		})
		if err != nil {
			log.Printf("[WARN] IPFS pin failed for bounty %s: %v", bid, err)
			return
		}
		ipfsCID = result.CID
		ipfsURL = result.GatewayURL
		// Update transaction log with IPFS CID
		database.DB.Exec(`
			UPDATE transaction_log SET ipfs_metadata_cid = $1, ipfs_gateway_url = $2
			WHERE bounty_id = $3 AND event = 'escrow_locked'
		`, ipfsCID, ipfsURL, bid)
	}()

	database.DB.Exec(`UPDATE bounties SET app_id = $1, escrow_txn_id = $2, updated_at = NOW() WHERE id = $3`, req.AppID, txID, bid)
	database.DB.Exec(`
		INSERT INTO transaction_log (bounty_id, actor_id, event, txn_id, txn_note, amount_algo)
		VALUES ($1, $2, 'escrow_locked', $3, $4, $5)
	`, bid, pid, txID, fmt.Sprintf("BountyVault:escrow_locked:%d", req.AppID), b.RewardAlgo)

	c.JSON(200, models.APIResponse{Success: true, Message: "Funds locked on-chain", Data: gin.H{"txn_id": txID, "app_id": req.AppID}})
}

// POST /api/bounties/:id/submit — Freelancer submits work file via multipart
func (h *BountyHandler) SubmitWork(c *gin.Context) {
	bid := c.Param("id")
	pid, _ := c.Get("profile_id")

	file, fileHeader, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(400, models.APIResponse{Success: false, Error: "File is required"})
		return
	}
	defer file.Close()

	description := c.PostForm("description")
	if strings.TrimSpace(description) == "" {
		c.JSON(400, models.APIResponse{Success: false, Error: "Description is required"})
		return
	}

	// Check bounty exists and is accepting submissions
	var status, creatorID string
	var maxS, remaining int
	var rewardAlgo float64
	var bountyDisplayID string
	var appID *int64
	err = database.DB.QueryRow(`
		SELECT status, creator_id, max_submissions, submissions_remaining, reward_algo, bounty_id, app_id
		FROM bounties WHERE id = $1
	`, bid).Scan(&status, &creatorID, &maxS, &remaining, &rewardAlgo, &bountyDisplayID, &appID)
	if err != nil {
		c.JSON(404, models.APIResponse{Success: false, Error: "Bounty not found"})
		return
	}
	if status != "open" && status != "in_progress" {
		c.JSON(400, models.APIResponse{Success: false, Error: "Bounty not accepting submissions"})
		return
	}
	if creatorID == pid.(string) {
		c.JSON(400, models.APIResponse{Success: false, Error: "Creator cannot submit work"})
		return
	}

	// Get submission number for this freelancer
	var subNum int
	database.DB.QueryRow(`SELECT COALESCE(MAX(submission_number), 0) + 1 FROM submissions WHERE bounty_id = $1 AND freelancer_id = $2`, bid, pid).Scan(&subNum)

	// Upload file to Cloudflare R2 with magic-byte validation
	r2Result, err := h.r2Svc.UploadSubmission(
		c.Request.Context(),
		pid.(string), bid, subNum,
		file, fileHeader.Filename, fileHeader.Size,
	)
	if err != nil {
		c.JSON(400, models.APIResponse{Success: false, Error: "File validation/upload failed: " + err.Error()})
		return
	}

	// Insert submission record
	sid := uuid.New()
	_, err = database.DB.Exec(`
		INSERT INTO submissions (id, bounty_id, freelancer_id, submission_number, file_url,
		  file_type, file_size_bytes, description, status, work_hash_sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', $9)
	`, sid, bid, pid, subNum, r2Result.Path, r2Result.FileType, r2Result.FileSize, description, r2Result.WorkHashSHA256)
	if err != nil {
		c.JSON(500, models.APIResponse{Success: false, Error: "Failed to record submission"})
		return
	}

	database.DB.Exec(`UPDATE bounties SET status = 'in_progress', updated_at = NOW() WHERE id = $1 AND status = 'open'`, bid)

	// Record in transaction_log
	appIDVal := int64(0)
	if appID != nil {
		appIDVal = *appID
	}
	database.DB.Exec(`
		INSERT INTO transaction_log (bounty_id, actor_id, event, txn_note, amount_algo)
		VALUES ($1, $2, 'work_submitted', $3, 0)
	`, bid, pid, fmt.Sprintf("BountyVault:work_submitted:%d", appIDVal))

	// Pin submission metadata to IPFS (fire-and-forget)
	go func() {
		var workerAddr string
		database.DB.QueryRow(`SELECT COALESCE(wallet_address,'') FROM profiles WHERE id = $1`, pid).Scan(&workerAddr)
		descPreview := description
		if len(descPreview) > 200 {
			descPreview = descPreview[:200] + "..."
		}
		_, err := h.ipfsSvc.PinWorkSubmitted(context.Background(), services.WorkSubmittedMetadata{
			BountyID:           bountyDisplayID,
			SubmissionNumber:   subNum,
			FreelancerAddress:  workerAddr,
			FileR2Path:         r2Result.Path,
			FileType:           r2Result.FileType,
			FileSizeBytes:      r2Result.FileSize,
			DescriptionPreview: descPreview,
			WorkHashSHA256:     r2Result.WorkHashSHA256,
			Network:            h.cfg.AlgoNetwork,
		})
		if err != nil {
			log.Printf("[WARN] IPFS pin failed for submission %s: %v", sid, err)
		}
	}()

	c.JSON(201, models.APIResponse{
		Success: true,
		Message: "Work submitted successfully",
		Data: gin.H{
			"submission_id":    sid,
			"submission_number": subNum,
			"file_type":        r2Result.FileType,
			"work_hash":        r2Result.WorkHashSHA256,
		},
	})
}

// PUT /api/bounties/:id/approve — Creator approves submission + payout
func (h *BountyHandler) ApproveSubmission(c *gin.Context) {
	bid := c.Param("id")
	pid, _ := c.Get("profile_id")
	var req models.ApproveSubmissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Error: err.Error()})
		return
	}

	var creatorID string
	var reward float64
	var bountyDisplayID string
	var appID *int64
	err := database.DB.QueryRow(`
		SELECT creator_id, reward_algo, bounty_id, app_id FROM bounties WHERE id = $1 AND status = 'in_progress'
	`, bid).Scan(&creatorID, &reward, &bountyDisplayID, &appID)
	if err != nil {
		c.JSON(404, models.APIResponse{Success: false, Error: "Bounty not found or not in progress"})
		return
	}
	if creatorID != pid.(string) {
		c.JSON(403, models.APIResponse{Success: false, Error: "Only creator can approve"})
		return
	}

	// Submit approval transaction to Algorand if txns are provided
	txID := "mock_approve_txn_" + bid
	if len(req.SignedTxns) > 0 {
		txID, err = h.algoSvc.SubmitSignedTxns(c.Request.Context(), req.SignedTxns)
		if err != nil {
			c.JSON(500, models.APIResponse{Success: false, Error: "Blockchain transaction failed: " + err.Error()})
			return
		}
		h.algoSvc.WaitForConfirmation(c.Request.Context(), txID, 10)
	}

	// Update submission
	var freelancerID string
	var fileURL string
	err = database.DB.QueryRow(`
		UPDATE submissions SET status = 'approved', creator_message = $1, creator_rating = $2, resolved_at = NOW()
		WHERE id = $3 AND bounty_id = $4 AND status = 'pending'
		RETURNING freelancer_id, file_url
	`, req.Message, req.Rating, req.SubmissionID, bid).Scan(&freelancerID, &fileURL)
	if err != nil {
		c.JSON(404, models.APIResponse{Success: false, Error: "Submission not found"})
		return
	}

	database.DB.Exec(`UPDATE bounties SET status = 'completed', payout_txn_id = $1, updated_at = NOW() WHERE id = $2`, txID, bid)
	database.DB.Exec(`
		UPDATE profiles SET
		  total_bounties_completed = total_bounties_completed + 1,
		  total_earned_algo = total_earned_algo + $1,
		  streak_count = streak_count + 1,
		  reputation_score = reputation_score + 10
		WHERE id = $2
	`, reward, freelancerID)

	// Log transaction
	appIDVal := int64(0)
	if appID != nil {
		appIDVal = *appID
	}
	database.DB.Exec(`
		INSERT INTO transaction_log (bounty_id, actor_id, event, txn_id, txn_note, amount_algo)
		VALUES ($1, $2, 'submission_approved', $3, $4, $5)
	`, bid, pid, txID, fmt.Sprintf("BountyVault:submission_approved:%d", appIDVal), reward)

	// Pin approval metadata to IPFS (fire-and-forget)
	go func() {
		var creatorAddr, freelancerAddr string
		database.DB.QueryRow(`SELECT COALESCE(wallet_address,'') FROM profiles WHERE id = $1`, pid).Scan(&creatorAddr)
		database.DB.QueryRow(`SELECT COALESCE(wallet_address,'') FROM profiles WHERE id = $1`, freelancerID).Scan(&freelancerAddr)
		_, err := h.ipfsSvc.PinSubmissionApproved(context.Background(), services.SubmissionApprovedMetadata{
			BountyID:          bountyDisplayID,
			SubmissionID:      req.SubmissionID,
			FreelancerAddress: freelancerAddr,
			CreatorAddress:    creatorAddr,
			RewardPaidAlgo:    reward,
			CreatorRating:     req.Rating,
			CreatorMessage:    req.Message,
			PayoutTxnID:       txID,
			Network:           h.cfg.AlgoNetwork,
		})
		if err != nil {
			log.Printf("[WARN] IPFS pin failed for approval: %v", err)
		}
	}()

	c.JSON(200, models.APIResponse{Success: true, Message: "Submission approved and ALGO transferred", Data: gin.H{
		"payout_txn_id": txID,
		"reward_algo":   reward,
		"freelancer_id": freelancerID,
	}})
}

// PUT /api/bounties/:id/reject — Creator rejects submission (min 50 char feedback)
func (h *BountyHandler) RejectSubmission(c *gin.Context) {
	bid := c.Param("id")
	pid, _ := c.Get("profile_id")
	var req models.RejectSubmissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Error: err.Error()})
		return
	}

	// Enforce 50-char minimum feedback
	if len(strings.TrimSpace(req.Feedback)) < 50 {
		c.JSON(400, models.APIResponse{Success: false, Error: "Rejection feedback must be at least 50 characters"})
		return
	}

	var creatorID string
	var bountyDisplayID string
	var appID *int64
	database.DB.QueryRow(`SELECT creator_id, bounty_id, app_id FROM bounties WHERE id = $1`, bid).Scan(&creatorID, &bountyDisplayID, &appID)
	if creatorID != pid.(string) {
		c.JSON(403, models.APIResponse{Success: false, Error: "Only creator can reject"})
		return
	}

	// Submit rejection transaction if txns are provided
	txID := "mock_reject_txn_" + bid
	var err error
	if len(req.SignedTxns) > 0 {
		txID, err = h.algoSvc.SubmitSignedTxns(c.Request.Context(), req.SignedTxns)
		if err != nil {
			c.JSON(500, models.APIResponse{Success: false, Error: "Transaction failed: " + err.Error()})
			return
		}
		h.algoSvc.WaitForConfirmation(c.Request.Context(), txID, 10)
	}

	// Count this rejection number
	var rejNum int
	database.DB.QueryRow(`SELECT COUNT(*) + 1 FROM submissions WHERE bounty_id = $1 AND status = 'rejected'`, bid).Scan(&rejNum)

	var freelancerID string
	err = database.DB.QueryRow(`
		UPDATE submissions SET status = 'rejected', rejection_feedback = $1, submission_txn_id = $2, resolved_at = NOW()
		WHERE id = $3 AND bounty_id = $4 AND status = 'pending'
		RETURNING freelancer_id
	`, req.Feedback, txID, req.SubmissionID, bid).Scan(&freelancerID)
	if err != nil {
		c.JSON(404, models.APIResponse{Success: false, Error: "Submission not found"})
		return
	}

	// Decrement submissions_remaining
	var remaining int
	database.DB.QueryRow(`
		UPDATE bounties SET submissions_remaining = submissions_remaining - 1, updated_at = NOW()
		WHERE id = $1 RETURNING submissions_remaining
	`, bid).Scan(&remaining)

	// Log transaction
	appIDVal := int64(0)
	if appID != nil {
		appIDVal = *appID
	}
	database.DB.Exec(`
		INSERT INTO transaction_log (bounty_id, actor_id, event, txn_id, txn_note)
		VALUES ($1, $2, 'submission_rejected', $3, $4)
	`, bid, pid, txID, fmt.Sprintf("BountyVault:submission_rejected:%d", appIDVal))

	// Pin rejection metadata to IPFS (fire-and-forget)
	feedbackPreview := req.Feedback
	if len(feedbackPreview) > 200 {
		feedbackPreview = feedbackPreview[:200] + "..."
	}
	go func() {
		_, err := h.ipfsSvc.PinSubmissionRejected(context.Background(), services.SubmissionRejectedMetadata{
			BountyID:                  bountyDisplayID,
			SubmissionID:              req.SubmissionID,
			RejectionNumber:           rejNum,
			SubmissionsRemainingAfter: remaining,
			FeedbackPreview:           feedbackPreview,
			TxnID:                     txID,
			Network:                   h.cfg.AlgoNetwork,
		})
		if err != nil {
			log.Printf("[WARN] IPFS pin failed for rejection: %v", err)
		}
	}()

	c.JSON(200, models.APIResponse{
		Success: true,
		Message: "Submission rejected",
		Data: gin.H{
			"submissions_remaining": remaining,
			"exhausted":             remaining == 0,
		},
	})
}

// POST /api/bounties/:id/dispute — Freelancer raises a DAO dispute (300 word min)
func (h *BountyHandler) InitiateDispute(c *gin.Context) {
	bid := c.Param("id")
	pid, _ := c.Get("profile_id")
	var req models.RaiseDisputeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Error: err.Error()})
		return
	}

	// Enforce 300-word minimum server-side
	wordCount := len(strings.Fields(req.Description))
	if wordCount < 300 {
		c.JSON(400, models.APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Dispute description must be at least 300 words (you have %d)", wordCount),
		})
		return
	}

	// Verify bounty and freelancer eligibility
	var status, creatorID string
	var bountyDisplayID string
	var rewardAlgo float64
	var appID *int64
	err := database.DB.QueryRow(`SELECT status, creator_id, bounty_id, reward_algo, app_id FROM bounties WHERE id = $1`, bid).Scan(
		&status, &creatorID, &bountyDisplayID, &rewardAlgo, &appID,
	)
	if err != nil {
		c.JSON(404, models.APIResponse{Success: false, Error: "Bounty not found"})
		return
	}
	// Must be expired (all rejections exhausted)
	if status != "expired" {
		c.JSON(400, models.APIResponse{Success: false, Error: "Dispute only allowed when all submission slots are exhausted"})
		return
	}
	if creatorID == pid.(string) {
		c.JSON(400, models.APIResponse{Success: false, Error: "Creator cannot raise a dispute"})
		return
	}

	// Submit initiate_dispute transaction
	txID := "mock_dispute_txn_" + bid
	if len(req.SignedTxns) > 0 {
		txID, err = h.algoSvc.SubmitSignedTxns(c.Request.Context(), req.SignedTxns)
		if err != nil {
			c.JSON(500, models.APIResponse{Success: false, Error: "Blockchain transaction failed: " + err.Error()})
			return
		}
		h.algoSvc.WaitForConfirmation(c.Request.Context(), txID, 10)
	}

	// Collect full submission history for IPFS metadata
	subHistory := []services.SubmissionHistoryEntry{}
	rows, _ := database.DB.Query(`
		SELECT submission_number, file_url, description, COALESCE(rejection_feedback,''), created_at, resolved_at
		FROM submissions WHERE bounty_id = $1 AND freelancer_id = $2 ORDER BY submission_number ASC
	`, bid, pid)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var entry services.SubmissionHistoryEntry
			var createdAt time.Time
			var resolvedAt *time.Time
			rows.Scan(&entry.Attempt, &entry.FileR2Path, &entry.Description, &entry.RejectionFeedback, &createdAt, &resolvedAt)
			entry.SubmittedAt = createdAt.Format(time.RFC3339)
			if resolvedAt != nil {
				entry.RejectedAt = resolvedAt.Format(time.RFC3339)
			}
			subHistory = append(subHistory, entry)
		}
	}

	subHistoryJSON, _ := json.Marshal(subHistory)
	votingDeadline := time.Now().Add(48 * time.Hour)
	did := uuid.New()

	// Generate dispute ID
	var disputeDisplayID string
	database.DB.QueryRow(`SELECT generate_dispute_id()`).Scan(&disputeDisplayID)

	_, err = database.DB.Exec(`
		INSERT INTO disputes (id, dispute_id, bounty_id, freelancer_id, creator_id,
		  freelancer_description, submission_history, status, voting_deadline)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'open', $8)
	`, did, disputeDisplayID, bid, pid, creatorID, req.Description, subHistoryJSON, votingDeadline)
	if err != nil {
		c.JSON(500, models.APIResponse{Success: false, Error: "Failed to create dispute record"})
		return
	}

	database.DB.Exec(`UPDATE bounties SET status = 'disputed', updated_at = NOW() WHERE id = $1`, bid)

	appIDVal := int64(0)
	if appID != nil {
		appIDVal = *appID
	}
	database.DB.Exec(`
		INSERT INTO transaction_log (bounty_id, actor_id, event, txn_id, txn_note)
		VALUES ($1, $2, 'dispute_raised', $3, $4)
	`, bid, pid, txID, fmt.Sprintf("BountyVault:dispute_raised:%d", appIDVal))

	// Pin dispute metadata to IPFS (fire-and-forget)
	go func() {
		var freelancerAddr string
		database.DB.QueryRow(`SELECT COALESCE(wallet_address,'') FROM profiles WHERE id = $1`, pid).Scan(&freelancerAddr)
		result, err := h.ipfsSvc.PinDisputeRaised(context.Background(), services.DisputeRaisedMetadata{
			DisputeID:           disputeDisplayID,
			BountyID:            bountyDisplayID,
			FreelancerAddress:   freelancerAddr,
			DisputeDescription:  req.Description,
			SubmissionHistory:   subHistory,
			VotingDeadline:      votingDeadline.Format(time.RFC3339),
			TxnID:               txID,
			Network:             h.cfg.AlgoNetwork,
		})
		if err != nil {
			log.Printf("[WARN] IPFS pin failed for dispute: %v", err)
			return
		}
		database.DB.Exec(`UPDATE disputes SET ipfs_dispute_cid = $1 WHERE id = $2`, result.CID, did)
	}()

	c.JSON(201, models.APIResponse{
		Success: true,
		Message: "Dispute raised — DAO Court is now open for 48 hours",
		Data: gin.H{
			"dispute_id":     disputeDisplayID,
			"voting_deadline": votingDeadline,
		},
	})
}

// POST /api/bounties/:id/letgo — Freelancer forfeits, creator refunded
func (h *BountyHandler) LetGoBounty(c *gin.Context) {
	bid := c.Param("id")
	pid, _ := c.Get("profile_id")
	var req models.LetGoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Error: err.Error()})
		return
	}

	var status, creatorID string
	var rewardAlgo float64
	var bountyDisplayID string
	var appID *int64
	err := database.DB.QueryRow(`SELECT status, creator_id, reward_algo, bounty_id, app_id FROM bounties WHERE id = $1`, bid).Scan(
		&status, &creatorID, &rewardAlgo, &bountyDisplayID, &appID,
	)
	if err != nil {
		c.JSON(404, models.APIResponse{Success: false, Error: "Bounty not found"})
		return
	}
	if status != "expired" {
		c.JSON(400, models.APIResponse{Success: false, Error: "Let Go only available when submission slots are exhausted"})
		return
	}
	if creatorID == pid.(string) {
		c.JSON(400, models.APIResponse{Success: false, Error: "Creator cannot use Let Go"})
		return
	}

	// Submit let_go_bounty transaction
	txID := "mock_let_go_txn_" + bid
	if len(req.SignedTxns) > 0 {
		txID, err = h.algoSvc.SubmitSignedTxns(c.Request.Context(), req.SignedTxns)
		if err != nil {
			c.JSON(500, models.APIResponse{Success: false, Error: "Transaction failed: " + err.Error()})
			return
		}
		h.algoSvc.WaitForConfirmation(c.Request.Context(), txID, 10)
	}

	database.DB.Exec(`UPDATE bounties SET status = 'cancelled', updated_at = NOW() WHERE id = $1`, bid)

	appIDVal := int64(0)
	if appID != nil {
		appIDVal = *appID
	}
	database.DB.Exec(`
		INSERT INTO transaction_log (bounty_id, actor_id, event, txn_id, txn_note, amount_algo)
		VALUES ($1, $2, 'freelancer_letgo', $3, $4, $5)
	`, bid, pid, txID, fmt.Sprintf("BountyVault:freelancer_letgo:%d", appIDVal), rewardAlgo)

	// Pin let-go metadata to IPFS
	go func() {
		var freelancerAddr, creatorAddr string
		database.DB.QueryRow(`SELECT COALESCE(wallet_address,'') FROM profiles WHERE id = $1`, pid).Scan(&freelancerAddr)
		database.DB.QueryRow(`SELECT COALESCE(wallet_address,'') FROM profiles WHERE id = $1`, creatorID).Scan(&creatorAddr)
		_, err := h.ipfsSvc.PinFreelancerLetGo(context.Background(), services.FreelancerLetGoMetadata{
			BountyID:          bountyDisplayID,
			FreelancerAddress: freelancerAddr,
			CreatorAddress:    creatorAddr,
			RefundedAlgo:      rewardAlgo,
			TxnID:             txID,
			Network:           h.cfg.AlgoNetwork,
		})
		if err != nil {
			log.Printf("[WARN] IPFS pin failed for letgo: %v", err)
		}
	}()

	c.JSON(200, models.APIResponse{
		Success: true,
		Message: "Bounty forfeited — creator will receive refund",
		Data:    gin.H{"txn_id": txID},
	})
}

// GET /api/bounties/:id/submissions — List submissions for a bounty
func (h *BountyHandler) ListSubmissions(c *gin.Context) {
	bid := c.Param("id")
	pid, _ := c.Get("profile_id")
	role, _ := c.Get("role")

	var query string
	var args []interface{}

	if role.(string) == "creator" {
		// Creator sees all submissions for their bounty
		query = `
			SELECT s.id, s.freelancer_id, s.submission_number, s.file_url, s.file_type,
			       s.file_size_bytes, s.description, s.status, s.rejection_feedback,
			       s.creator_message, s.creator_rating, s.work_hash_sha256, s.created_at,
			       p.username, p.display_name, p.avatar_url, p.reputation_score
			FROM submissions s JOIN profiles p ON s.freelancer_id = p.id
			WHERE s.bounty_id = $1 ORDER BY s.created_at DESC`
		args = []interface{}{bid}
	} else {
		// Freelancer sees only their own submissions
		query = `
			SELECT s.id, s.freelancer_id, s.submission_number, s.file_url, s.file_type,
			       s.file_size_bytes, s.description, s.status, s.rejection_feedback,
			       s.creator_message, s.creator_rating, s.work_hash_sha256, s.created_at,
			       p.username, p.display_name, p.avatar_url, p.reputation_score
			FROM submissions s JOIN profiles p ON s.freelancer_id = p.id
			WHERE s.bounty_id = $1 AND s.freelancer_id = $2 ORDER BY s.created_at DESC`
		args = []interface{}{bid, pid}
	}

	rows, err := database.DB.Query(query, args...)
	if err != nil {
		c.JSON(500, models.APIResponse{Success: false, Error: "Failed to fetch submissions"})
		return
	}
	defer rows.Close()

	subs := []gin.H{}
	for rows.Next() {
		var sid, flID string
		var subNum, fileSize, rep int
		var fileURL, fileType, desc, status, hash string
		var feedback, msg *string
		var rating *int
		var createdAt time.Time
		var un, dn string
		var av *string

		rows.Scan(
			&sid, &flID, &subNum, &fileURL, &fileType, &fileSize, &desc, &status,
			&feedback, &msg, &rating, &hash, &createdAt,
			&un, &dn, &av, &rep,
		)

		// Generate signed URL for file access
		signedURL, _ := h.r2Svc.GenerateSignedURL(c.Request.Context(), fileURL)

		subs = append(subs, gin.H{
			"id": sid, "freelancer_id": flID, "submission_number": subNum,
			"file_type": fileType, "file_size_bytes": fileSize,
			"description": desc, "status": status, "rejection_feedback": feedback,
			"creator_message": msg, "creator_rating": rating,
			"work_hash_sha256": hash, "created_at": createdAt,
			"signed_file_url": signedURL,
			"freelancer": gin.H{"username": un, "display_name": dn, "avatar_url": av, "reputation_score": rep},
		})
	}
	c.JSON(200, models.APIResponse{Success: true, Data: subs})
}

// POST /api/bounties/:id/cancel
func (h *BountyHandler) CancelBounty(c *gin.Context) {
	bid := c.Param("id")
	pid, _ := c.Get("profile_id")

	var creatorID string
	var subCount int
	err := database.DB.QueryRow(`
		SELECT creator_id, (SELECT COUNT(*) FROM submissions WHERE bounty_id = $1)
		FROM bounties WHERE id = $1 AND status = 'open'
	`, bid).Scan(&creatorID, &subCount)
	if err != nil {
		c.JSON(404, models.APIResponse{Success: false, Error: "Bounty not found or not cancellable"})
		return
	}
	if creatorID != pid.(string) {
		c.JSON(403, models.APIResponse{Success: false, Error: "Only creator can cancel"})
		return
	}
	if subCount > 0 {
		c.JSON(400, models.APIResponse{Success: false, Error: "Cannot cancel bounty with submissions"})
		return
	}

	database.DB.Exec(`UPDATE bounties SET status = 'cancelled', updated_at = NOW() WHERE id = $1`, bid)
	c.JSON(200, models.APIResponse{Success: true, Message: "Bounty cancelled"})
}

// POST /api/bounties/:id/refund-expired — Permissionless expired refund trigger
func (h *BountyHandler) RefundExpired(c *gin.Context) {
	bid := c.Param("id")

	var status string
	var deadline time.Time
	err := database.DB.QueryRow(`SELECT status, deadline FROM bounties WHERE id = $1`, bid).Scan(&status, &deadline)
	if err != nil {
		c.JSON(404, models.APIResponse{Success: false, Error: "Bounty not found"})
		return
	}
	if status != "open" && status != "in_progress" {
		c.JSON(400, models.APIResponse{Success: false, Error: "Bounty not refundable in current status"})
		return
	}
	if time.Now().Before(deadline) {
		c.JSON(400, models.APIResponse{Success: false, Error: "Deadline not yet passed"})
		return
	}

	database.DB.Exec(`UPDATE bounties SET status = 'expired', updated_at = NOW() WHERE id = $1`, bid)
	database.DB.Exec(`
		INSERT INTO transaction_log (bounty_id, event, txn_note)
		VALUES ($1, 'bounty_expired', 'BountyVault:bounty_expired:deadline_passed')
	`, bid)

	c.JSON(200, models.APIResponse{Success: true, Message: "Bounty marked expired — refund triggered on-chain"})
}

// POST /api/bounties/:id/rate — Creator rates a worker after approval
func (h *BountyHandler) RateWorker(c *gin.Context) {
	bid := c.Param("id")
	pid, _ := c.Get("profile_id")
	var req struct {
		WorkerID string `json:"worker_id" binding:"required"`
		Stars    int    `json:"stars" binding:"required,min=1,max=5"`
		Comment  string `json:"comment"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Error: err.Error()})
		return
	}

	var cid string
	database.DB.QueryRow(`SELECT creator_id FROM bounties WHERE id=$1 AND status='completed'`, bid).Scan(&cid)
	if cid != pid.(string) {
		c.JSON(403, models.APIResponse{Success: false, Error: "Only creator can rate"})
		return
	}

	_, err := database.DB.Exec(`INSERT INTO ratings (bounty_id,rater_id,worker_id,stars,comment) VALUES ($1,$2,$3,$4,$5)`,
		bid, pid, req.WorkerID, req.Stars, req.Comment)
	if err != nil {
		c.JSON(409, models.APIResponse{Success: false, Error: "Already rated"})
		return
	}

	c.JSON(201, models.APIResponse{Success: true, Message: "Worker rated"})
}

// GET /api/categories — List bounty categories
func (h *BountyHandler) ListCategories(c *gin.Context) {
	rows, _ := database.DB.Query(`SELECT id, name, slug, description, icon, color FROM bounty_categories ORDER BY name`)
	if rows == nil {
		c.JSON(200, models.APIResponse{Success: true, Data: []gin.H{}})
		return
	}
	defer rows.Close()
	var cats []gin.H
	for rows.Next() {
		var id int
		var n, s, i, cl string
		var d *string
		rows.Scan(&id, &n, &s, &d, &i, &cl)
		cats = append(cats, gin.H{"id": id, "name": n, "slug": s, "description": d, "icon": i, "color": cl})
	}
	c.JSON(200, models.APIResponse{Success: true, Data: cats})
}

// ============================================================
// Bounty Acceptance Handlers (v3.2)
// ============================================================

// POST /api/bounties/:id/accept — Freelancer requests to accept a bounty
func (h *BountyHandler) AcceptBounty(c *gin.Context) {
	bid := c.Param("id")
	pid, _ := c.Get("profile_id")
	role, _ := c.Get("role")

	if role.(string) != "freelancer" {
		c.JSON(403, models.APIResponse{Success: false, Error: "Only freelancers can accept bounties"})
		return
	}

	var req models.AcceptBountyRequest
	// Message is optional, so bind silently
	c.ShouldBindJSON(&req)

	// Verify bounty exists and is open
	var creatorID, status string
	var title string
	err := database.DB.QueryRow(`SELECT creator_id, status, title FROM bounties WHERE id = $1`, bid).Scan(&creatorID, &status, &title)
	if err != nil {
		c.JSON(404, models.APIResponse{Success: false, Error: "Bounty not found"})
		return
	}
	if status != "open" {
		c.JSON(400, models.APIResponse{Success: false, Error: "Bounty is not open for acceptance"})
		return
	}
	if creatorID == pid.(string) {
		c.JSON(400, models.APIResponse{Success: false, Error: "Creator cannot accept their own bounty"})
		return
	}

	// Check for duplicate
	var existingID string
	err = database.DB.QueryRow(`SELECT id FROM bounty_acceptances WHERE bounty_id = $1 AND freelancer_id = $2`, bid, pid).Scan(&existingID)
	if err == nil {
		c.JSON(409, models.APIResponse{Success: false, Error: "You have already requested to accept this bounty"})
		return
	}

	// Insert acceptance request
	aid := uuid.New()
	var msg *string
	if req.Message != "" {
		msg = &req.Message
	}
	_, err = database.DB.Exec(`
		INSERT INTO bounty_acceptances (id, bounty_id, freelancer_id, status, message)
		VALUES ($1, $2, $3, 'pending', $4)
	`, aid, bid, pid, msg)
	if err != nil {
		c.JSON(500, models.APIResponse{Success: false, Error: "Failed to create acceptance request"})
		return
	}

	// Notify the creator
	var freelancerUsername string
	database.DB.QueryRow(`SELECT username FROM profiles WHERE id = $1`, pid).Scan(&freelancerUsername)
	database.DB.Exec(`
		INSERT INTO notifications (user_id, type, title, message, bounty_id)
		VALUES ($1, 'acceptance_request', 'New Acceptance Request',
			$2, $3)
	`, creatorID, fmt.Sprintf("%s wants to work on your bounty: %s", freelancerUsername, title), bid)

	c.JSON(201, models.APIResponse{
		Success: true,
		Message: "Acceptance request sent to creator",
		Data:    gin.H{"acceptance_id": aid},
	})
}

// GET /api/bounties/:id/acceptances — List acceptance requests for a bounty
func (h *BountyHandler) GetAcceptances(c *gin.Context) {
	bid := c.Param("id")
	pid, _ := c.Get("profile_id")

	// Verify the caller is the creator of this bounty
	var creatorID string
	err := database.DB.QueryRow(`SELECT creator_id FROM bounties WHERE id = $1`, bid).Scan(&creatorID)
	if err != nil {
		c.JSON(404, models.APIResponse{Success: false, Error: "Bounty not found"})
		return
	}

	// Allow both creator (sees all) and freelancers (see own)
	var rows *sql.Rows
	if creatorID == pid.(string) {
		rows, err = database.DB.Query(`
			SELECT a.id, a.bounty_id, a.freelancer_id, a.status, a.message, a.creator_note, a.created_at, a.updated_at,
			       p.id, p.username, p.display_name, p.avatar_url, p.reputation_score, p.bio,
			       p.total_bounties_completed, p.avg_rating, p.total_ratings
			FROM bounty_acceptances a
			JOIN profiles p ON a.freelancer_id = p.id
			WHERE a.bounty_id = $1
			ORDER BY a.created_at DESC
		`, bid)
	} else {
		rows, err = database.DB.Query(`
			SELECT a.id, a.bounty_id, a.freelancer_id, a.status, a.message, a.creator_note, a.created_at, a.updated_at,
			       p.id, p.username, p.display_name, p.avatar_url, p.reputation_score, p.bio,
			       p.total_bounties_completed, p.avg_rating, p.total_ratings
			FROM bounty_acceptances a
			JOIN profiles p ON a.freelancer_id = p.id
			WHERE a.bounty_id = $1 AND a.freelancer_id = $2
			ORDER BY a.created_at DESC
		`, bid, pid)
	}
	if err != nil {
		c.JSON(500, models.APIResponse{Success: false, Error: "Failed to fetch acceptances"})
		return
	}
	defer rows.Close()

	acceptances := []gin.H{}
	for rows.Next() {
		var aID, aBountyID, aFreelancerID, aStatus string
		var aMsg, aNote *string
		var aCreated, aUpdated time.Time
		var pID, pUsername string
		var pDisplayName, pAvatarURL, pBio *string
		var pRep, pCompleted, pTotalRatings int
		var pAvgRating float64

		rows.Scan(
			&aID, &aBountyID, &aFreelancerID, &aStatus, &aMsg, &aNote, &aCreated, &aUpdated,
			&pID, &pUsername, &pDisplayName, &pAvatarURL, &pRep, &pBio,
			&pCompleted, &pAvgRating, &pTotalRatings,
		)

		acceptances = append(acceptances, gin.H{
			"id":           aID,
			"bounty_id":    aBountyID,
			"freelancer_id": aFreelancerID,
			"status":       aStatus,
			"message":      aMsg,
			"creator_note": aNote,
			"created_at":   aCreated,
			"updated_at":   aUpdated,
			"freelancer": gin.H{
				"id":                       pID,
				"username":                 pUsername,
				"display_name":             pDisplayName,
				"avatar_url":               pAvatarURL,
				"reputation_score":         pRep,
				"bio":                      pBio,
				"total_bounties_completed": pCompleted,
				"avg_rating":               pAvgRating,
				"total_ratings":            pTotalRatings,
			},
		})
	}

	c.JSON(200, models.APIResponse{Success: true, Data: acceptances})
}

// PUT /api/bounties/:id/review-acceptance — Creator reviews (approve/reject) a freelancer's acceptance
func (h *BountyHandler) ReviewAcceptance(c *gin.Context) {
	bid := c.Param("id")
	pid, _ := c.Get("profile_id")

	var req models.ReviewAcceptanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Error: err.Error()})
		return
	}

	if req.Action != "approve" && req.Action != "reject" {
		c.JSON(400, models.APIResponse{Success: false, Error: "Action must be 'approve' or 'reject'"})
		return
	}

	// Verify bounty ownership
	var creatorID string
	var rewardAlgo float64
	var deadline time.Time
	var maxSubs int
	var bountyStatus string
	err := database.DB.QueryRow(`
		SELECT creator_id, reward_algo, deadline, max_submissions, status
		FROM bounties WHERE id = $1
	`, bid).Scan(&creatorID, &rewardAlgo, &deadline, &maxSubs, &bountyStatus)
	if err != nil {
		c.JSON(404, models.APIResponse{Success: false, Error: "Bounty not found"})
		return
	}
	if creatorID != pid.(string) {
		c.JSON(403, models.APIResponse{Success: false, Error: "Only the creator can review acceptances"})
		return
	}
	if bountyStatus != "open" {
		c.JSON(400, models.APIResponse{Success: false, Error: "Bounty is not in open state"})
		return
	}

	// Verify acceptance exists and is pending
	var acceptanceID string
	err = database.DB.QueryRow(`
		SELECT id FROM bounty_acceptances
		WHERE bounty_id = $1 AND freelancer_id = $2 AND status = 'pending'
	`, bid, req.FreelancerID).Scan(&acceptanceID)
	if err != nil {
		c.JSON(404, models.APIResponse{Success: false, Error: "Pending acceptance request not found"})
		return
	}

	if req.Action == "reject" {
		var note *string
		if req.Note != "" {
			note = &req.Note
		}
		database.DB.Exec(`
			UPDATE bounty_acceptances SET status = 'rejected', creator_note = $1, updated_at = NOW()
			WHERE id = $2
		`, note, acceptanceID)

		// Notify freelancer
		database.DB.Exec(`
			INSERT INTO notifications (user_id, type, title, message, bounty_id)
			VALUES ($1, 'acceptance_rejected', 'Acceptance Request Rejected',
				'Your acceptance request was rejected by the creator.', $2)
		`, req.FreelancerID, bid)

		c.JSON(200, models.APIResponse{Success: true, Message: "Acceptance rejected"})
		return
	}

	// Action == "approve" — need to build escrow contract txns
	if req.WalletAddress == "" {
		c.JSON(400, models.APIResponse{Success: false, Error: "wallet_address is required for approval (to lock escrow)"})
		return
	}

	// Build unsigned escrow transactions for Pera Wallet
	txns, err := h.algoSvc.BuildCreateBountyTxns(
		c.Request.Context(), req.WalletAddress,
		uint64(rewardAlgo*1e6), nil, uint64(deadline.Unix()), uint64(maxSubs), "",
	)
	if err != nil {
		c.JSON(500, models.APIResponse{Success: false, Error: "Failed to build escrow transactions: " + err.Error()})
		return
	}

	c.JSON(200, models.APIResponse{
		Success: true,
		Message: "Sign the escrow transactions with Pera Wallet to approve this freelancer",
		Data: gin.H{
			"transactions":  txns,
			"freelancer_id": req.FreelancerID,
			"acceptance_id": acceptanceID,
		},
	})
}

// POST /api/bounties/:id/confirm-acceptance — Submit signed escrow txns and finalize acceptance
func (h *BountyHandler) ConfirmAcceptance(c *gin.Context) {
	bid := c.Param("id")
	pid, _ := c.Get("profile_id")

	var req models.ConfirmAcceptanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, models.APIResponse{Success: false, Error: err.Error()})
		return
	}

	// Verify bounty ownership
	var creatorID string
	var rewardAlgo float64
	var bountyDisplayID string
	err := database.DB.QueryRow(`
		SELECT creator_id, reward_algo, bounty_id FROM bounties WHERE id = $1 AND status = 'open'
	`, bid).Scan(&creatorID, &rewardAlgo, &bountyDisplayID)
	if err != nil {
		c.JSON(404, models.APIResponse{Success: false, Error: "Bounty not found or not in open state"})
		return
	}
	if creatorID != pid.(string) {
		c.JSON(403, models.APIResponse{Success: false, Error: "Only the creator can confirm acceptance"})
		return
	}

	// Submit signed transactions to Algorand
	txID, err := h.algoSvc.SubmitSignedTxns(c.Request.Context(), req.SignedTxns)
	if err != nil {
		c.JSON(500, models.APIResponse{Success: false, Error: "Transaction submission failed: " + err.Error()})
		return
	}
	h.algoSvc.WaitForConfirmation(c.Request.Context(), txID, 10)

	// Update bounty: set app_id, escrow_txn_id, status -> in_progress
	database.DB.Exec(`
		UPDATE bounties SET app_id = $1, escrow_txn_id = $2, status = 'in_progress', updated_at = NOW()
		WHERE id = $3
	`, req.AppID, txID, bid)

	// Update acceptance: approved
	database.DB.Exec(`
		UPDATE bounty_acceptances SET status = 'approved', updated_at = NOW()
		WHERE bounty_id = $1 AND freelancer_id = $2 AND status = 'pending'
	`, bid, req.FreelancerID)

	// Reject all other pending acceptances for this bounty
	database.DB.Exec(`
		UPDATE bounty_acceptances SET status = 'rejected', creator_note = 'Another freelancer was selected', updated_at = NOW()
		WHERE bounty_id = $1 AND freelancer_id != $2 AND status = 'pending'
	`, bid, req.FreelancerID)

	// Log transaction
	database.DB.Exec(`
		INSERT INTO transaction_log (bounty_id, actor_id, event, txn_id, txn_note, amount_algo)
		VALUES ($1, $2, 'escrow_locked', $3, $4, $5)
	`, bid, pid, txID, fmt.Sprintf("BountyVault:escrow_locked:%d", req.AppID), rewardAlgo)

	// Notify freelancer
	database.DB.Exec(`
		INSERT INTO notifications (user_id, type, title, message, bounty_id)
		VALUES ($1, 'acceptance_approved', 'You have been selected!',
			'Your acceptance request was approved. You can now submit work on this bounty.', $2)
	`, req.FreelancerID, bid)

	// Pin to IPFS (fire-and-forget)
	go func() {
		var walletAddr string
		database.DB.QueryRow(`SELECT COALESCE(wallet_address,'') FROM profiles WHERE id = $1`, pid).Scan(&walletAddr)
		var b models.Bounty
		database.DB.QueryRow(`SELECT bounty_id, reward_algo, max_submissions, deadline, tags FROM bounties WHERE id = $1`, bid).Scan(
			&b.BountyID, &b.RewardAlgo, &b.MaxSubmissions, &b.Deadline, pq.Array(&b.Tags),
		)
		_, pinErr := h.ipfsSvc.PinBountyCreated(context.Background(), services.BountyCreatedMetadata{
			BountyID:       b.BountyID,
			AppID:          req.AppID,
			CreatorAddress: walletAddr,
			RewardAlgo:     b.RewardAlgo,
			MaxSubmissions: b.MaxSubmissions,
			Deadline:       b.Deadline.Format(time.RFC3339),
			Tags:           b.Tags,
			TxnID:          txID,
			Network:        h.cfg.AlgoNetwork,
		})
		if pinErr != nil {
			log.Printf("[WARN] IPFS pin failed for acceptance confirmation: %v", pinErr)
		}
	}()

	c.JSON(200, models.APIResponse{
		Success: true,
		Message: "Escrow locked and freelancer approved",
		Data:    gin.H{"txn_id": txID, "app_id": req.AppID},
	})
}

// GET /api/bounties/:id/my-acceptance — Get the current freelancer's acceptance status for a bounty
func (h *BountyHandler) GetMyAcceptanceStatus(c *gin.Context) {
	bid := c.Param("id")
	pid, _ := c.Get("profile_id")

	var aID, aStatus string
	var aMsg, aNote *string
	var aCreated time.Time
	err := database.DB.QueryRow(`
		SELECT id, status, message, creator_note, created_at
		FROM bounty_acceptances
		WHERE bounty_id = $1 AND freelancer_id = $2
	`, bid, pid).Scan(&aID, &aStatus, &aMsg, &aNote, &aCreated)
	if err != nil {
		c.JSON(200, models.APIResponse{Success: true, Data: nil})
		return
	}

	c.JSON(200, models.APIResponse{Success: true, Data: gin.H{
		"id":           aID,
		"status":       aStatus,
		"message":      aMsg,
		"creator_note": aNote,
		"created_at":   aCreated,
	}})
}


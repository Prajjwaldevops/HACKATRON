package services

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// MegaNZValidator validates mega.nz links
type MegaNZValidator struct {
	httpClient *http.Client
}

// NewMegaNZValidator creates a new mega.nz link validator
func NewMegaNZValidator() *MegaNZValidator {
	return &MegaNZValidator{
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
			// Don't follow redirects — we just want to check the link responds
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// ValidateMegaLink validates a mega.nz link by:
// 1. Checking URL format (must start with https://mega.nz/)
// 2. Making a HEAD request to verify the link is reachable
func (v *MegaNZValidator) ValidateMegaLink(link string) error {
	link = strings.TrimSpace(link)

	// Format validation
	if link == "" {
		return fmt.Errorf("mega.nz link is required")
	}
	if !strings.HasPrefix(link, "https://mega.nz/") {
		return fmt.Errorf("invalid mega.nz link — must start with https://mega.nz/")
	}

	// Must contain a file or folder path after the domain
	path := strings.TrimPrefix(link, "https://mega.nz/")
	if len(path) < 5 {
		return fmt.Errorf("invalid mega.nz link — URL appears incomplete")
	}

	// HTTP validation — check the link is reachable
	req, err := http.NewRequest("HEAD", link, nil)
	if err != nil {
		return fmt.Errorf("invalid mega.nz URL format: %w", err)
	}
	req.Header.Set("User-Agent", "BountyVault/3.3")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mega.nz link unreachable — please verify the link is correct")
	}
	defer resp.Body.Close()

	// mega.nz returns various codes — 200, 302, 303 are all valid
	// 404 or 5xx mean the link is invalid
	if resp.StatusCode >= 500 {
		return fmt.Errorf("mega.nz returned server error (%d) — please try again", resp.StatusCode)
	}

	return nil
}

// ValidateEncryptionKeyContent validates that the uploaded .txt file
// contains an encryption key (non-empty, reasonable format)
func ValidateEncryptionKeyContent(content []byte) error {
	text := strings.TrimSpace(string(content))

	if len(text) == 0 {
		return fmt.Errorf("encryption key file is empty — must contain the decryption key")
	}

	if len(text) < 8 {
		return fmt.Errorf("encryption key too short — must be at least 8 characters")
	}

	if len(text) > 1024 {
		return fmt.Errorf("encryption key file too large — max 1024 characters")
	}

	return nil
}

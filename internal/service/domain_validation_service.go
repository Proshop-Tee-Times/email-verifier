package service

import (
	"context"
	"sync"
)

// ConcurrentDomainValidationService handles concurrent domain validation operations
type ConcurrentDomainValidationService struct {
	domainValidator DomainValidator
}

// NewConcurrentDomainValidationService creates a new instance of ConcurrentDomainValidationService
func NewConcurrentDomainValidationService(validator DomainValidator) *ConcurrentDomainValidationService {
	return &ConcurrentDomainValidationService{
		domainValidator: validator,
	}
}

// ValidateDomainConcurrently runs domain validation checks concurrently
// For email validation, domain_exists is derived from MX records (or A record fallback per RFC 5321).
// A domain "exists" for email purposes if it can receive email, not if it has an A record.
func (s *ConcurrentDomainValidationService) ValidateDomainConcurrently(ctx context.Context, domain string) (exists, hasMX, isDisposable bool) {
	// Check if context is already done before starting
	select {
	case <-ctx.Done():
		return false, false, false
	default:
		// Continue with validation
	}

	var wg sync.WaitGroup
	wg.Add(3)

	// Channel for collecting validation results
	results := make(chan struct {
		validationType string
		isValid        bool
	}, 3)

	// Run A record check (used as fallback for domain existence per RFC 5321)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			results <- struct {
				validationType string
				isValid        bool
			}{"has_a_record", false}
		default:
			isValid := s.domainValidator.ValidateDomain(domain)
			// Check context again after validation
			select {
			case <-ctx.Done():
				results <- struct {
					validationType string
					isValid        bool
				}{"has_a_record", false}
			default:
				results <- struct {
					validationType string
					isValid        bool
				}{"has_a_record", isValid}
			}
		}
	}()

	// Run MX records check
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			results <- struct {
				validationType string
				isValid        bool
			}{"mx_records", false}
		default:
			isValid := s.domainValidator.ValidateMXRecords(domain)
			// Check context again after validation
			select {
			case <-ctx.Done():
				results <- struct {
					validationType string
					isValid        bool
				}{"mx_records", false}
			default:
				results <- struct {
					validationType string
					isValid        bool
				}{"mx_records", isValid}
			}
		}
	}()

	// Run disposable domain check
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			results <- struct {
				validationType string
				isValid        bool
			}{"is_disposable", false}
		default:
			isValid := s.domainValidator.IsDisposable(domain)
			// Check context again after validation
			select {
			case <-ctx.Done():
				results <- struct {
					validationType string
					isValid        bool
				}{"is_disposable", false}
			default:
				results <- struct {
					validationType string
					isValid        bool
				}{"is_disposable", isValid}
			}
		}
	}()

	// Close results channel after all goroutines complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect validation results
	var hasARecord bool
	for result := range results {
		switch result.validationType {
		case "has_a_record":
			hasARecord = result.isValid
		case "mx_records":
			hasMX = result.isValid
		case "is_disposable":
			isDisposable = result.isValid
		}
	}

	// Final check if context was canceled
	select {
	case <-ctx.Done():
		return false, false, false
	default:
		// domain_exists for email purposes means: has MX records OR has A record (RFC 5321 fallback)
		// This fixes the bug where a domain with MX but no A record was marked as non-existent
		exists = hasMX || hasARecord
		return exists, hasMX, isDisposable
	}
}

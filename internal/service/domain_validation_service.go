package service

import (
	"context"
	"fmt"
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

// domainResult holds the result of a single domain validation check
type domainResult struct {
	validationType string
	isValid        bool
	err            error
}

// ValidateDomainConcurrently runs domain validation checks concurrently.
// Returns an error if any DNS lookup failed transiently (timeout, SERVFAIL, etc.).
// Definitive results (NXDOMAIN) are not errors.
func (s *ConcurrentDomainValidationService) ValidateDomainConcurrently(ctx context.Context, domain string) (exists, hasMX, isDisposable bool, err error) {
	// Check if context is already done before starting
	select {
	case <-ctx.Done():
		return false, false, false, ctx.Err()
	default:
	}

	var wg sync.WaitGroup
	wg.Add(3)

	results := make(chan domainResult, 3)

	// Run A record check
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			results <- domainResult{"has_a_record", false, ctx.Err()}
		default:
			isValid, lookupErr := s.domainValidator.ValidateDomain(domain)
			results <- domainResult{"has_a_record", isValid, lookupErr}
		}
	}()

	// Run MX records check
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			results <- domainResult{"mx_records", false, ctx.Err()}
		default:
			isValid, lookupErr := s.domainValidator.ValidateMXRecords(domain)
			results <- domainResult{"mx_records", isValid, lookupErr}
		}
	}()

	// Run disposable domain check (this is a local list lookup, never fails)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
			results <- domainResult{"is_disposable", false, ctx.Err()}
		default:
			isValid := s.domainValidator.IsDisposable(domain)
			results <- domainResult{"is_disposable", isValid, nil}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	var hasARecord bool
	var errs []error
	for result := range results {
		if result.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.validationType, result.err))
			continue
		}
		switch result.validationType {
		case "has_a_record":
			hasARecord = result.isValid
		case "mx_records":
			hasMX = result.isValid
		case "is_disposable":
			isDisposable = result.isValid
		}
	}

	// If any DNS lookup had a transient failure, return an error — do not cache
	if len(errs) > 0 {
		return false, false, false, fmt.Errorf("DNS lookup failed for %s: %w", domain, errs[0])
	}

	// Final check if context was canceled
	select {
	case <-ctx.Done():
		return false, false, false, ctx.Err()
	default:
		// Both DNS lookups succeeded — cache the combined result
		s.domainValidator.CacheDomainResult(domain, hasARecord, hasMX)
		exists = hasMX || hasARecord
		return exists, hasMX, isDisposable, nil
	}
}

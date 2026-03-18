package validator

import (
	"errors"
	"net"
	"sync/atomic"
	"time"

	"emailvalidator/pkg/monitoring"
)

// DomainValidator handles domain existence validation
type DomainValidator struct {
	resolver       DNSResolver
	cacheManager   *DomainCacheManager
	cleanupRunning atomic.Bool
}

// NewDomainValidator creates a new instance of DomainValidator
func NewDomainValidator(resolver DNSResolver, cacheManager *DomainCacheManager) *DomainValidator {
	return &DomainValidator{
		resolver:     resolver,
		cacheManager: cacheManager,
	}
}

// isDNSNotFound returns true if the error indicates the domain definitively does not exist (NXDOMAIN).
// Transient errors (timeouts, SERVFAIL, network issues) return false.
func isDNSNotFound(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsNotFound
	}
	return false
}

// Validate checks if the domain exists by performing an A record lookup.
// Returns (exists, error). A non-nil error means the lookup failed transiently
// and the result should not be trusted or cached.
func (v *DomainValidator) Validate(domain string) (bool, error) {
	// Check cache first
	if entry, found := v.cacheManager.Get(domain); found {
		monitoring.RecordCacheOperation("domain_a_lookup", "hit")
		return entry.HasARecord, nil
	}
	monitoring.RecordCacheOperation("domain_a_lookup", "miss")

	// Perform lookup
	start := time.Now()
	_, err := v.resolver.LookupHost(domain)
	monitoring.RecordDNSLookup("host", time.Since(start))

	if err != nil {
		if isDNSNotFound(err) {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

// ValidateMX checks if the domain has valid MX records.
// Returns (hasMX, error). A non-nil error means the lookup failed transiently
// and the result should not be trusted.
func (v *DomainValidator) ValidateMX(domain string) (bool, error) {
	// Check cache first
	if entry, found := v.cacheManager.Get(domain); found {
		monitoring.RecordCacheOperation("domain_mx_lookup", "hit")
		return entry.HasMX, nil
	}
	monitoring.RecordCacheOperation("domain_mx_lookup", "miss")

	start := time.Now()
	mxRecords, err := v.resolver.LookupMX(domain)
	monitoring.RecordDNSLookup("mx", time.Since(start))

	if err != nil {
		if isDNSNotFound(err) {
			return false, nil
		}
		return false, err
	}

	if len(mxRecords) == 0 {
		return false, nil
	}

	// Check for null MX record (RFC 7505)
	if len(mxRecords) == 1 && mxRecords[0].Host == "." {
		return false, nil
	}

	return true, nil
}

// CacheDomainResult stores both A record and MX results for a domain.
// Only call this when both lookups succeeded (no transient errors).
func (v *DomainValidator) CacheDomainResult(domain string, hasARecord, hasMX bool) {
	v.cacheManager.Set(domain, DomainCacheEntry{
		HasARecord: hasARecord,
		HasMX:      hasMX,
	})
	v.triggerCleanup()
}

func (v *DomainValidator) triggerCleanup() {
	if v.cleanupRunning.CompareAndSwap(false, true) {
		go func() {
			defer v.cleanupRunning.Store(false)
			v.cacheManager.ClearExpired()
		}()
	}
}

// Package servicetest contains unit tests for the domain validation service
package servicetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"emailvalidator/internal/service"
	"emailvalidator/tests/unit/service/mocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestConcurrentDomainValidationService_ValidateDomainConcurrently(t *testing.T) {
	tests := []struct {
		name            string
		domain          string
		timeout         time.Duration
		setup           func(*mocks.MockDomainValidator)
		expectedExists  bool
		expectedHasMX   bool
		expectedDispose bool
		expectError     bool
	}{
		{
			name:    "All validations pass",
			domain:  "example.com",
			timeout: 5 * time.Second,
			setup: func(mv *mocks.MockDomainValidator) {
				mv.On("ValidateDomain", "example.com").Return(true, nil)
				mv.On("ValidateMXRecords", "example.com").Return(true, nil)
				mv.On("IsDisposable", "example.com").Return(false)
				mv.On("CacheDomainResult", "example.com", true, true)
			},
			expectedExists:  true,
			expectedHasMX:   true,
			expectedDispose: false,
		},
		{
			name:    "Domain does not exist - no MX and no A record",
			domain:  "nonexistent.com",
			timeout: 5 * time.Second,
			setup: func(mv *mocks.MockDomainValidator) {
				mv.On("ValidateDomain", "nonexistent.com").Return(false, nil)
				mv.On("ValidateMXRecords", "nonexistent.com").Return(false, nil)
				mv.On("IsDisposable", "nonexistent.com").Return(false)
				mv.On("CacheDomainResult", "nonexistent.com", false, false)
			},
			expectedExists:  false,
			expectedHasMX:   false,
			expectedDispose: false,
		},
		{
			name:    "Disposable email domain",
			domain:  "temp.com",
			timeout: 5 * time.Second,
			setup: func(mv *mocks.MockDomainValidator) {
				mv.On("ValidateDomain", "temp.com").Return(true, nil)
				mv.On("ValidateMXRecords", "temp.com").Return(true, nil)
				mv.On("IsDisposable", "temp.com").Return(true)
				mv.On("CacheDomainResult", "temp.com", true, true)
			},
			expectedExists:  true,
			expectedHasMX:   true,
			expectedDispose: true,
		},
		{
			name:    "Context timeout",
			domain:  "slow.com",
			timeout: 1 * time.Millisecond,
			setup: func(mv *mocks.MockDomainValidator) {
				mv.On("ValidateDomain", "slow.com").After(10 * time.Millisecond).Return(true, nil)
				mv.On("ValidateMXRecords", "slow.com").After(10 * time.Millisecond).Return(true, nil)
				mv.On("IsDisposable", "slow.com").After(10 * time.Millisecond).Return(false)
				// CacheDomainResult may or may not be called depending on timing
				mv.On("CacheDomainResult", mock.Anything, mock.Anything, mock.Anything).Maybe()
			},
			expectedExists:  false,
			expectedHasMX:   false,
			expectedDispose: false,
			expectError:     true,
		},
		{
			name:    "MX records exist but no A record - domain exists for email (RFC 5321)",
			domain:  "mx-only.com",
			timeout: 5 * time.Second,
			setup: func(mv *mocks.MockDomainValidator) {
				mv.On("ValidateDomain", "mx-only.com").Return(false, nil)   // No A record
				mv.On("ValidateMXRecords", "mx-only.com").Return(true, nil) // Has MX
				mv.On("IsDisposable", "mx-only.com").Return(false)
				mv.On("CacheDomainResult", "mx-only.com", false, true)
			},
			expectedExists:  true, // Should be true because MX exists
			expectedHasMX:   true,
			expectedDispose: false,
		},
		{
			name:    "A record exists but no MX records - domain exists via fallback (RFC 5321)",
			domain:  "a-only.com",
			timeout: 5 * time.Second,
			setup: func(mv *mocks.MockDomainValidator) {
				mv.On("ValidateDomain", "a-only.com").Return(true, nil)     // Has A record
				mv.On("ValidateMXRecords", "a-only.com").Return(false, nil) // No MX
				mv.On("IsDisposable", "a-only.com").Return(false)
				mv.On("CacheDomainResult", "a-only.com", true, false)
			},
			expectedExists:  true, // Should be true because A record exists (fallback)
			expectedHasMX:   false,
			expectedDispose: false,
		},
		{
			name:    "DNS lookup failure returns error - no caching",
			domain:  "flaky.com",
			timeout: 5 * time.Second,
			setup: func(mv *mocks.MockDomainValidator) {
				mv.On("ValidateDomain", "flaky.com").Return(false, errors.New("DNS timeout"))
				mv.On("ValidateMXRecords", "flaky.com").Return(true, nil)
				mv.On("IsDisposable", "flaky.com").Return(false)
				// CacheDomainResult should NOT be called when there's an error
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup mock
			mockValidator := new(mocks.MockDomainValidator)
			tt.setup(mockValidator)

			// Create service
			svc := service.NewConcurrentDomainValidationService(mockValidator)

			// Create context with timeout
			ctx, cancel := context.WithTimeout(context.Background(), tt.timeout)
			defer cancel()

			// Execute
			exists, hasMX, isDisposable, err := svc.ValidateDomainConcurrently(ctx, tt.domain)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedExists, exists)
				assert.Equal(t, tt.expectedHasMX, hasMX)
				assert.Equal(t, tt.expectedDispose, isDisposable)
			}

			// Verify mock expectations
			mockValidator.AssertExpectations(t)
		})
	}
}

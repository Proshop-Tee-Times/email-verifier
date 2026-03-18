// Package service implements the core business logic of the email validator service.
// It provides email validation, batch processing, and typo suggestion functionality.
package service

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"emailvalidator/internal/model"
	"emailvalidator/pkg/cache"
	"emailvalidator/pkg/validator"
)

// EmailService handles email validation operations
type EmailService struct {
	emailRuleValidator  EmailRuleValidator
	domainValidator     DomainValidator
	domainValidationSvc DomainValidationService
	batchValidationSvc  *BatchValidationService
	metricsCollector    MetricsCollector
	startTime           time.Time
	requests            int64
}

// NewEmailService creates a new instance of EmailService without Redis cache
func NewEmailService() (*EmailService, error) {
	return NewEmailServiceWithCache(nil)
}

// NewEmailServiceWithCache creates a new instance of EmailService with optional Redis cache
func NewEmailServiceWithCache(redisCache cache.Cache) (*EmailService, error) {
	emailValidator, err := validator.NewEmailValidatorWithCache(redisCache)
	if err != nil {
		return nil, err
	}

	metricsAdapter := NewMetricsAdapter()
	domainValidationSvc := NewConcurrentDomainValidationService(emailValidator)
	batchValidationSvc := NewBatchValidationService(emailValidator, domainValidationSvc, metricsAdapter)

	return &EmailService{
		emailRuleValidator:  emailValidator,
		domainValidator:     emailValidator,
		domainValidationSvc: domainValidationSvc,
		batchValidationSvc:  batchValidationSvc,
		metricsCollector:    metricsAdapter,
		startTime:           time.Now(),
	}, nil
}

// NewEmailServiceWithDeps creates a new instance of EmailService with custom dependencies.
// The validator must implement both EmailRuleValidator and DomainValidator interfaces.
// This is primarily used for testing.
func NewEmailServiceWithDeps(validator interface{}) *EmailService {
	emailRuleValidator, _ := validator.(EmailRuleValidator)
	domainValidator, _ := validator.(DomainValidator)

	if emailRuleValidator == nil {
		panic("validator must implement EmailRuleValidator interface")
	}
	if domainValidator == nil {
		panic("validator must implement DomainValidator interface")
	}

	metricsAdapter := NewMetricsAdapter()
	domainValidationSvc := NewConcurrentDomainValidationService(domainValidator)
	batchValidationSvc := NewBatchValidationService(emailRuleValidator, domainValidationSvc, metricsAdapter)

	return &EmailService{
		emailRuleValidator:  emailRuleValidator,
		domainValidator:     domainValidator,
		domainValidationSvc: domainValidationSvc,
		batchValidationSvc:  batchValidationSvc,
		metricsCollector:    metricsAdapter,
		startTime:           time.Now(),
	}
}

// ValidateEmail performs all validation checks on a single email.
// Returns an error if domain DNS lookups failed transiently — the caller
// should treat this as "unable to validate" rather than "invalid".
func (s *EmailService) ValidateEmail(email string) (model.EmailValidationResponse, error) {
	atomic.AddInt64(&s.requests, 1)

	response := model.EmailValidationResponse{
		Email:       email,
		Validations: model.ValidationResults{},
	}

	if email == "" {
		response.Status = model.ValidationStatusMissingEmail
		return response, nil
	}

	// Validate syntax first
	response.Validations.Syntax = s.emailRuleValidator.ValidateSyntax(email)
	if !response.Validations.Syntax {
		response.Status = model.ValidationStatusInvalidFormat
		return response, nil
	}

	// Extract domain and validate
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		response.Status = model.ValidationStatusInvalidFormat
		return response, nil
	}
	domain := parts[1]

	// Perform domain validations concurrently
	exists, hasMX, isDisposable, err := s.domainValidationSvc.ValidateDomainConcurrently(context.Background(), domain)
	if err != nil {
		return response, err
	}

	// Set validation results
	response.Validations.DomainExists = exists
	response.Validations.MXRecords = hasMX
	response.Validations.IsDisposable = isDisposable
	response.Validations.IsRoleBased = s.emailRuleValidator.IsRoleBased(email)

	// Always check for typo suggestions
	suggestions := s.emailRuleValidator.GetTypoSuggestions(email)
	if len(suggestions) > 0 {
		response.TypoSuggestion = suggestions[0]
	}

	// Detect if email is an alias
	if canonicalEmail := s.emailRuleValidator.DetectAlias(email); canonicalEmail != "" && canonicalEmail != email {
		response.AliasOf = canonicalEmail
	}

	// Calculate score
	validationMap := map[string]bool{
		"syntax":        response.Validations.Syntax,
		"domain_exists": response.Validations.DomainExists,
		"mx_records":    response.Validations.MXRecords,
		"is_disposable": response.Validations.IsDisposable,
		"is_role_based": response.Validations.IsRoleBased,
	}
	response.Score = s.emailRuleValidator.CalculateScore(validationMap)

	// Reduce score if there's a typo suggestion
	if response.TypoSuggestion != "" {
		response.Score = max(0, response.Score-20) // Ensure score doesn't go below 0
	}

	// Set status based on validations
	switch {
	case !response.Validations.DomainExists:
		response.Status = model.ValidationStatusInvalidDomain
		response.Score = 40
	case !response.Validations.MXRecords:
		response.Status = model.ValidationStatusNoMXRecords
		response.Score = 40
	case response.Validations.IsDisposable:
		response.Status = model.ValidationStatusDisposable
	case response.Score >= 90:
		response.Status = model.ValidationStatusValid
	case response.Score >= 70:
		response.Status = model.ValidationStatusProbablyValid
	default:
		response.Status = model.ValidationStatusInvalid
	}

	// Record validation score after status override
	s.metricsCollector.RecordValidationScore("overall", float64(response.Score))

	return response, nil
}

// ValidateEmails performs validation on multiple email addresses concurrently
func (s *EmailService) ValidateEmails(emails []string) model.BatchValidationResponse {
	atomic.AddInt64(&s.requests, 1)
	return s.batchValidationSvc.ValidateEmails(emails)
}

// GetTypoSuggestions returns suggestions for possible email typos
func (s *EmailService) GetTypoSuggestions(email string) model.TypoSuggestionResponse {
	atomic.AddInt64(&s.requests, 1)
	suggestions := s.emailRuleValidator.GetTypoSuggestions(email)
	response := model.TypoSuggestionResponse{
		Email: email,
	}
	if len(suggestions) > 0 {
		response.TypoSuggestion = suggestions[0]
	}
	return response
}

// GetAPIStatus returns the current status of the API
func (s *EmailService) GetAPIStatus() model.APIStatus {
	uptime := time.Since(s.startTime)

	// Update memory metrics
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	s.metricsCollector.UpdateMemoryUsage(float64(m.HeapInuse), float64(m.StackInuse))

	return model.APIStatus{
		Status:            "healthy",
		Uptime:            uptime.String(),
		RequestsHandled:   atomic.LoadInt64(&s.requests),
		AvgResponseTimeMs: 0, // TODO: calculate from actual request metrics
	}
}

// SetDomainValidationService sets the domain validation service (for testing)
func (s *EmailService) SetDomainValidationService(svc DomainValidationService) {
	s.domainValidationSvc = svc
}

// SetMetricsCollector sets the metrics collector (for testing)
func (s *EmailService) SetMetricsCollector(collector MetricsCollector) {
	s.metricsCollector = collector
}

// SetBatchValidationService sets the batch validation service (for testing)
func (s *EmailService) SetBatchValidationService(svc *BatchValidationService) {
	s.batchValidationSvc = svc
}

// SetEmailRuleValidator sets the email rule validator (for testing)
func (s *EmailService) SetEmailRuleValidator(validator EmailRuleValidator) {
	s.emailRuleValidator = validator
}

// SetDomainValidator sets the domain validator (for testing)
func (s *EmailService) SetDomainValidator(validator DomainValidator) {
	s.domainValidator = validator
}

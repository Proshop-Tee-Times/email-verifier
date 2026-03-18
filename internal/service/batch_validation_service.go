package service

import (
	"context"
	"runtime"
	"strings"
	"sync"

	"emailvalidator/internal/model"
	"emailvalidator/internal/utils"
)

// BatchValidationService handles batch email validation operations
type BatchValidationService struct {
	emailRuleValidator   EmailRuleValidator
	domainValidationSvc  DomainValidationService
	metricsCollector     MetricsCollector
	maxConcurrentWorkers int
}

// NewBatchValidationService creates a new instance of BatchValidationService
func NewBatchValidationService(
	ruleValidator EmailRuleValidator,
	domainValidationSvc DomainValidationService,
	metricsCollector MetricsCollector,
) *BatchValidationService {
	return &BatchValidationService{
		emailRuleValidator:   ruleValidator,
		domainValidationSvc:  domainValidationSvc,
		metricsCollector:     metricsCollector,
		maxConcurrentWorkers: runtime.NumCPU() * 4,
	}
}

// domainValidationResult holds the result of a domain validation, including any error
type domainValidationResult struct {
	DomainExists bool
	MXRecords    bool
	IsDisposable bool
	Err          error
}

// ValidateEmails performs validation on multiple email addresses concurrently.
// Emails whose domain DNS lookups fail transiently are excluded from results.
func (s *BatchValidationService) ValidateEmails(emails []string) model.BatchValidationResponse {
	if len(emails) == 0 {
		return model.BatchValidationResponse{Results: []model.EmailValidationResponse{}}
	}

	// Group emails by domain
	emailsByDomain := s.groupEmailsByDomain(emails)

	// Process domain validations
	domainResults := s.processDomainValidations(emailsByDomain)

	// Process individual emails, excluding those with DNS errors
	response := s.processEmails(emails, emailsByDomain, domainResults)

	return response
}

func (s *BatchValidationService) groupEmailsByDomain(emails []string) map[string][]string {
	emailsByDomain := make(map[string][]string)
	for _, email := range emails {
		if email == "" {
			continue
		}

		parts := strings.Split(email, "@")
		if len(parts) != 2 {
			continue
		}

		domain := parts[1]
		emailsByDomain[domain] = append(emailsByDomain[domain], email)
	}
	return emailsByDomain
}

func (s *BatchValidationService) processDomainValidations(emailsByDomain map[string][]string) map[string]domainValidationResult {
	ctx := context.Background()
	domainResults := make(map[string]domainValidationResult)

	var wg sync.WaitGroup
	resultChan := make(chan struct {
		domain string
		result domainValidationResult
	}, len(emailsByDomain))

	// Process domains concurrently
	for domain := range emailsByDomain {
		wg.Add(1)
		go func(d string) {
			defer wg.Done()
			exists, hasMX, isDisposable, err := s.domainValidationSvc.ValidateDomainConcurrently(ctx, d)
			resultChan <- struct {
				domain string
				result domainValidationResult
			}{d, domainValidationResult{exists, hasMX, isDisposable, err}}
		}(domain)
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	for result := range resultChan {
		domainResults[result.domain] = result.result
	}

	return domainResults
}

func (s *BatchValidationService) processEmails(
	emails []string,
	emailsByDomain map[string][]string,
	domainResults map[string]domainValidationResult,
) model.BatchValidationResponse {
	var response model.BatchValidationResponse
	resultsMap := make(map[string]model.EmailValidationResponse)

	// Identify which emails can be validated (domain lookup succeeded)
	var validatable []string
	for _, email := range emails {
		if email == "" {
			// Empty emails get a response (MISSING_EMAIL), no domain lookup needed
			validatable = append(validatable, email)
			continue
		}
		parts := strings.Split(email, "@")
		if len(parts) != 2 {
			// Invalid format emails get a response, no domain lookup needed
			validatable = append(validatable, email)
			continue
		}
		domain := parts[1]
		domainResult, exists := domainResults[domain]
		if !exists || domainResult.Err != nil {
			// Domain lookup failed — exclude this email from results
			continue
		}
		validatable = append(validatable, email)
	}

	if len(validatable) == 0 {
		return model.BatchValidationResponse{Results: []model.EmailValidationResponse{}}
	}

	jobs := make(chan string, len(validatable))
	results := make(chan model.EmailValidationResponse, len(validatable))

	// Start workers
	workerCount := utils.MinInt(len(validatable), s.maxConcurrentWorkers)
	var wg sync.WaitGroup
	wg.Add(workerCount)

	for i := 0; i < workerCount; i++ {
		go s.emailValidationWorker(&wg, jobs, results, domainResults)
	}

	// Send jobs
	for _, email := range validatable {
		jobs <- email
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	for result := range results {
		resultsMap[result.Email] = result
	}

	// Preserve original order, excluding emails with DNS errors
	for _, email := range validatable {
		response.Results = append(response.Results, resultsMap[email])
	}

	return response
}

func (s *BatchValidationService) emailValidationWorker(
	wg *sync.WaitGroup,
	jobs <-chan string,
	results chan<- model.EmailValidationResponse,
	domainResults map[string]domainValidationResult,
) {
	defer wg.Done()

	for email := range jobs {
		response := s.validateSingleEmail(email, domainResults)
		results <- response
	}
}

func (s *BatchValidationService) validateSingleEmail(
	email string,
	domainResults map[string]domainValidationResult,
) model.EmailValidationResponse {
	response := model.EmailValidationResponse{
		Email:       email,
		Validations: model.ValidationResults{},
	}

	if email == "" {
		response.Status = model.ValidationStatusMissingEmail
		return response
	}

	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		response.Status = model.ValidationStatusInvalidFormat
		return response
	}

	domain := parts[1]
	response.Validations.Syntax = s.emailRuleValidator.ValidateSyntax(email)
	if !response.Validations.Syntax {
		response.Status = model.ValidationStatusInvalidFormat
		return response
	}

	// Get domain validation results
	domainValidation := domainResults[domain]
	response.Validations.DomainExists = domainValidation.DomainExists
	response.Validations.MXRecords = domainValidation.MXRecords
	response.Validations.IsDisposable = domainValidation.IsDisposable
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
		response.Score = max(0, response.Score-20)
	}

	// Set status
	response.Status = s.determineValidationStatus(&response)

	// Record validation score after status override
	s.metricsCollector.RecordValidationScore("overall", float64(response.Score))

	return response
}

func (s *BatchValidationService) determineValidationStatus(response *model.EmailValidationResponse) model.ValidationStatus {
	switch {
	case !response.Validations.DomainExists:
		response.Score = 40
		return model.ValidationStatusInvalidDomain
	case !response.Validations.MXRecords:
		response.Score = 40
		return model.ValidationStatusNoMXRecords
	case response.Validations.IsDisposable:
		return model.ValidationStatusDisposable
	case response.Score >= 90:
		return model.ValidationStatusValid
	case response.Score >= 70:
		return model.ValidationStatusProbablyValid
	default:
		return model.ValidationStatusInvalid
	}
}

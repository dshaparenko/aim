package common

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/andygrunwald/go-jira"
	sre "github.com/devopsext/sre/common"
)

// JiraOptions holds Jira connection settings
type JiraOptions struct {
	URL             string
	Username        string
	ApiToken        string
	Password        string
	ProjectKey      string
	QueryFilter     string
	RefreshInterval int
}

// JiraClient represents a wrapper around go-jira client with metrics and logging
type JiraClient struct {
	client          *jira.Client
	baseURL         string
	username        string
	projectKey      string
	queryFilter     string
	refreshInterval int
	obs             *Observability
	metrics         *sre.Metrics
	mu              sync.RWMutex
	lastRefresh     time.Time
	issueCache      map[string]*jira.Issue
}

// JiraIssue represents an issue with custom fields
type JiraIssue struct {
	Key             string    `json:"key"`
	Created         time.Time `json:"created"`
	Updated         time.Time `json:"updated"`
	Resolved        time.Time `json:"resolved,omitezero"`
	Assignee        string    `json:"assignee,omitempty"`
	Closed          time.Time `json:"closed,omitezero"`
	Head            string    `json:"head,omitempty"`
	Started         time.Time `json:"started,omitezero"`
	Firefighting    time.Time `json:"firefighting,omitezero"`
	Fixed           time.Time `json:"fixed,omitezero"`
	Severity        string    `json:"severity,omitempty"`
	Service         string    `json:"service,omitempty"`
	RootCause       string    `json:"root_cause,omitempty"`
	Regions         string    `json:"regions,omitempty"`
	Recovery        string    `json:"recovery,omitempty"`
	Reporter        string    `json:"reporter,omitempty"`
	Detected        time.Time `json:"detected,omitezero"`
	Escalated       time.Time `json:"escalated,omitezero"`
	Metrics         string    `json:"metrics,omitempty"`
	IssueType       string    `json:"issuetype,omitempty"`
	Environment     string    `json:"environment,omitempty"`
	Application     string    `json:"application,omitempty"`
	BusinessProcess string    `json:"businessprocess,omitempty"`
	Score           int       `json:"score,omitempty"`
}

func NewJiraClient(baseURL, username, apiToken, projectKey, queryFilter string, refreshInterval int, obs *Observability, metrics *sre.Metrics) (*JiraClient, error) {
	tp := jira.BasicAuthTransport{
		Username: username,
		Password: apiToken,
	}

	client, err := jira.NewClient(tp.Client(), baseURL)
	if err != nil {
		return nil, fmt.Errorf("error creating jira client: %w", err)
	}

	return &JiraClient{
		client:          client,
		baseURL:         baseURL,
		username:        username,
		projectKey:      projectKey,
		queryFilter:     queryFilter,
		refreshInterval: refreshInterval,
		obs:             obs,
		metrics:         metrics,
		issueCache:      make(map[string]*jira.Issue),
	}, nil
}

// GetIssues retrieves issues from Jira based on project key and filters similar to the old implementation
func (j *JiraClient) GetIssues(ctx context.Context) ([]*jira.Issue, error) {
	startTime := time.Now()

	// Default JQL similar to the old implementation
	jql := fmt.Sprintf("project = %s AND status not in (Cancelled,Rejected) AND created>=startOfYear(-1y) ORDER BY created DESC", j.projectKey)

	// Apply additional filter if specified
	if j.queryFilter != "" {
		jql = fmt.Sprintf("%s AND %s", jql, j.queryFilter)
	}

	j.obs.Info("Querying Jira with JQL: %s", jql)

	// Use pagination to get all issues, but try to get a larger batch size like the old implementation
	var allIssues []*jira.Issue
	startAt := 0
	maxResults := 1000 // Trying to match the old value of 100000 is unrealistic, most APIs cap at lower values

	for {
		options := &jira.SearchOptions{
			StartAt:    startAt,
			MaxResults: maxResults,
			Fields: []string{
				"key", "created", "updated", "resolutiondate", "assignee",
				"customfield_22501", "customfield_18117", "customfield_21200",
				"customfield_20908", "customfield_20905", "customfield_18119",
				"customfield_33803", "customfield_21501", "customfield_24800",
				"customfield_20911", "customfield_21201", "reporter",
				"customfield_31207", "customfield_31208", "issuetype",
				"customfield_29800", "customfield_28222", "customfield_32112",
				"customfield_30304", "customfield_37238",
			},
		}

		chunk, _, err := j.client.Issue.Search(jql, options)
		if err != nil {
			j.obs.Error("HTTP request failed: %v", err)
			return nil, fmt.Errorf("error searching issues: %w", err)
		}

		if len(chunk) == 0 {
			break
		}

		// Convert []jira.Issue to []*jira.Issue
		for i := range chunk {
			allIssues = append(allIssues, &chunk[i])
			j.updateIssueCache(&chunk[i])
		}

		if len(chunk) < maxResults {
			break
		}

		startAt += len(chunk)
	}
	// Record metric for API call duration
	if j.metrics != nil {
		if j.metrics != nil {
			j.obs.Info("API call duration: %f seconds", time.Since(startTime).Seconds())
		}
	}

	j.obs.Info("Retrieved %d issues from Jira", len(allIssues))
	return allIssues, nil
}

// ConvertToCustomIssues transforms jira.Issue objects into our custom JiraIssue format with the fields we care about
func (j *JiraClient) ConvertToCustomIssues(issues []*jira.Issue) ([]*JiraIssue, error) {
	customIssues := make([]*JiraIssue, 0, len(issues))

	for _, issue := range issues {
		customIssue := &JiraIssue{
			Key: issue.Key,
		}

		// Extract standard fields that are already in a usable format
		if issue.Fields.Assignee != nil {
			customIssue.Assignee = issue.Fields.Assignee.Name
		}

		if issue.Fields.Reporter != nil {
			customIssue.Reporter = issue.Fields.Reporter.Name
		}

		// Jira time fields come as jira.Time type which is already a time.Time
		customIssue.Created = time.Time(issue.Fields.Created)
		customIssue.Updated = time.Time(issue.Fields.Updated)

		if !time.Time(issue.Fields.Resolutiondate).IsZero() {
			customIssue.Resolved = time.Time(issue.Fields.Resolutiondate)
		}

		if issue.Fields.Type.Name != "" {
			customIssue.IssueType = issue.Fields.Type.Name
		}

		// Extract custom fields based on the old implementation
		// These will need adjustments based on your actual Jira instance
		// customfield_20908 (closed)
		if val, ok := issue.Fields.Unknowns["customfield_20908"].(string); ok && val != "" {
			if t, err := time.Parse("2006-01-02T15:04:05.999-0700", val); err == nil {
				customIssue.Closed = t
			}
		}

		// customfield_22501 (head)
		if val, ok := issue.Fields.Unknowns["customfield_22501"].(map[string]interface{}); ok {
			if name, ok := val["name"].(string); ok {
				customIssue.Head = name
			}
		}

		// customfield_18117 (started)
		if val, ok := issue.Fields.Unknowns["customfield_18117"].(string); ok && val != "" {
			if t, err := time.Parse("2006-01-02T15:04:05.999-0700", val); err == nil {
				customIssue.Started = t
			}
		}

		// customfield_21200 (firefighting)
		if val, ok := issue.Fields.Unknowns["customfield_21200"].(string); ok && val != "" {
			if t, err := time.Parse("2006-01-02T15:04:05.999-0700", val); err == nil {
				customIssue.Firefighting = t
			}
		}

		// More custom fields based on the old implementation
		// Severity
		if val, ok := issue.Fields.Unknowns["customfield_18119"].(map[string]interface{}); ok {
			if value, ok := val["value"].(string); ok {
				customIssue.Severity = value
			}
		}

		// Service
		if val, ok := issue.Fields.Unknowns["customfield_33803"].([]interface{}); ok && len(val) > 0 {
			if serviceVal, ok := val[0].(string); ok {
				customIssue.Service = serviceVal
			}
		}

		// Root Cause
		if val, ok := issue.Fields.Unknowns["customfield_37238"].([]interface{}); ok && len(val) > 0 {
			if causeVal, ok := val[0].(string); ok {
				customIssue.RootCause = causeVal
			}
		}

		customIssues = append(customIssues, customIssue)
	}

	return customIssues, nil
}

// updateIssueCache updates the local issue cache
func (j *JiraClient) updateIssueCache(issue *jira.Issue) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.issueCache[issue.ID] = issue
}

// StartRefreshLoop begins a loop to periodically refresh Jira data
func (j *JiraClient) StartRefreshLoop(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()

		ticker := time.NewTicker(time.Duration(j.refreshInterval) * time.Second)
		defer ticker.Stop()

		// Initial load
		j.RefreshData(ctx)

		// Periodic refresh
		for {
			select {
			case <-ctx.Done():
				j.obs.Info("Stopping Jira refresh loop due to context cancellation")
				return
			case <-ticker.C:
				j.RefreshData(ctx)
			}
		}
	}()
}

// RefreshData fetches the latest data from Jira
func (j *JiraClient) RefreshData(ctx context.Context) {
	j.obs.Info("Refreshing Jira data...")

	issues, err := j.GetIssues(ctx)
	if err != nil {
		j.obs.Error("Failed to refresh Jira data: %v", err)
		return
	}

	// Convert to custom issues with the fields we care about
	customIssues, err := j.ConvertToCustomIssues(issues)
	if err != nil {
		j.obs.Error("Failed to process Jira issues: %v", err)
		return
	}

	j.mu.Lock()
	j.lastRefresh = time.Now()
	j.mu.Unlock()

	j.obs.Info("Jira data refreshed successfully. Total issues: %d", len(customIssues))

	// Display some issue details for debugging
	if len(customIssues) > 0 {
		j.obs.Info("Latest issue: %s, created: %s",
			customIssues[0].Key,
			customIssues[0].Created.Format(time.RFC3339))
	}
}

// GetLastRefreshTime returns the timestamp of the last successful data refresh
func (j *JiraClient) GetLastRefreshTime() time.Time {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.lastRefresh
}

// TestConnection verifies connection to Jira
func (j *JiraClient) TestConnection() error {
	// The go-jira library doesnt have a Myself method, use the Current User API instead
	user, _, err := j.client.User.GetSelf()
	if err != nil {
		j.obs.Error("HTTP request failed: %v", err)
		return fmt.Errorf("jira connection test failed: %w", err)
	}

	j.obs.Info("Successfully connected to Jira as %s", user.Name)
	return nil
}

// reportHttpError logs HTTP response details on error
func (j *JiraClient) reportHttpError(resp *http.Response, err error) {
	if resp == nil {
		j.obs.Error("HTTP request failed with no response: %v", err)
		return
	}

	j.obs.Error("HTTP request failed - Status: %d, Error: %v", resp.StatusCode, err)

	// Record metric for API errors
	if j.metrics != nil {
		j.obs.Info("API error with status code: %d", resp.StatusCode)
	}
}

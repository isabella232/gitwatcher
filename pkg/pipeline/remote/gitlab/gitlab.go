package gitlab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-querystring/query"
	"github.com/pkg/errors"
	"github.com/rancher/norman/httperror"
	"github.com/rancher/rancher/pkg/ref"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/types/apis/project.cattle.io/v3"
	"github.com/rancher/webhookinator/pkg/pipeline/remote/model"
	"github.com/rancher/webhookinator/pkg/pipeline/utils"
	"github.com/rancher/webhookinator/types/apis/webhookinator.cattle.io/v1"
	"github.com/xanzy/go-gitlab"
)

const (
	defaultGitlabAPI  = "https://gitlab.com/api/v4"
	defaultGitlabHost = "gitlab.com"
	maxPerPage        = "100"
	gitlabAPI         = "%s%s/api/v4"
	descRunning       = "This build is running"
	descPending       = "This build is pending"
	descSuccess       = "This build is success"
	descFailure       = "This build is failure"
)

type client struct {
	Scheme       string
	Host         string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	API          string
}

func New(config *v3.GitlabPipelineConfig) (model.Remote, error) {
	if config == nil {
		return nil, errors.New("empty gitlab config")
	}
	glClient := &client{
		ClientID:     config.ClientID,
		ClientSecret: config.ClientSecret,
		RedirectURL:  config.RedirectURL,
	}
	if config.Hostname != "" && config.Hostname != defaultGitlabHost {
		glClient.Host = config.Hostname
		if config.TLS {
			glClient.Scheme = "https://"
		} else {
			glClient.Scheme = "http://"
		}
		glClient.API = fmt.Sprintf(gitlabAPI, glClient.Scheme, glClient.Host)
	} else {
		glClient.Scheme = "https://"
		glClient.Host = defaultGitlabHost
		glClient.API = defaultGitlabAPI
	}
	return glClient, nil
}

func (c *client) Type() string {
	return model.GitlabType
}

func (c *client) CreateHook(receiver *v1.GitWebHookReceiver, accessToken string) error {
	user, repo, err := getUserRepoFromURL(receiver.Spec.RepositoryURL)
	if err != nil {
		return err
	}
	project := url.QueryEscape(user + "/" + repo)
	hookURL := fmt.Sprintf("%s/%s%s", settings.ServerURL.Get(), utils.HooksEndpointPrefix, ref.Ref(receiver))
	opt := &gitlab.AddProjectHookOptions{
		PushEvents:            gitlab.Bool(true),
		MergeRequestsEvents:   gitlab.Bool(true),
		TagPushEvents:         gitlab.Bool(true),
		URL:                   gitlab.String(hookURL),
		EnableSSLVerification: gitlab.Bool(false),
		Token:                 gitlab.String(receiver.Status.Token),
	}
	url := fmt.Sprintf("%s/projects/%s/hooks", c.API, project)
	_, err = doRequestToGitlab(http.MethodPost, url, accessToken, opt)
	return err
}

func (c *client) DeleteHook(receiver *v1.GitWebHookReceiver, accessToken string) error {
	user, repo, err := getUserRepoFromURL(receiver.Spec.RepositoryURL)
	if err != nil {
		return err
	}
	project := url.QueryEscape(user + "/" + repo)

	hook, err := c.getHook(receiver, accessToken)
	if err != nil {
		return err
	}
	if hook != nil {
		url := fmt.Sprintf("%s/projects/%s/hooks/%v", c.API, project, hook.ID)
		resp, err := doRequestToGitlab(http.MethodDelete, url, accessToken, nil)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
	}
	return nil
}

func (c *client) UpdateStatus(execution *v1.GitWebHookExecution, accessToken string) error {
	user, repo, err := getUserRepoFromURL(execution.Spec.RepositoryURL)
	if err != nil {
		return err
	}
	project := url.QueryEscape(user + "/" + repo)
	status, desc := convertStatusDesc(execution)
	commit := execution.Spec.Commit
	opt := &gitlab.SetCommitStatusOptions{
		State:       status,
		Context:     gitlab.String(utils.StatusContext),
		TargetURL:   gitlab.String(execution.Status.StatusURL),
		Description: gitlab.String(desc),
	}
	url := fmt.Sprintf("%s/projects/%s/statuses/%s", c.API, project, commit)
	_, err = doRequestToGitlab(http.MethodPost, url, accessToken, opt)
	return err
}

func convertStatusDesc(execution *v1.GitWebHookExecution) (gitlab.BuildStateValue, string) {
	handleCondition := v1.GitWebHookExecutionConditionHandled.GetStatus(execution)
	switch handleCondition {
	case "Unknown":
		return gitlab.Running, descRunning
	case "True":
		return gitlab.Success, descSuccess
	case "False":
		return gitlab.Failed, descFailure
	default:
		return gitlab.Pending, descPending
	}
}

func (c *client) getHook(receiver *v1.GitWebHookReceiver, accessToken string) (*gitlab.ProjectHook, error) {
	user, repo, err := getUserRepoFromURL(receiver.Spec.RepositoryURL)
	if err != nil {
		return nil, err
	}
	project := url.QueryEscape(user + "/" + repo)

	var hooks []gitlab.ProjectHook
	var result *gitlab.ProjectHook
	url := fmt.Sprintf(c.API+"/projects/%s/hooks", project)
	resp, err := getFromGitlab(accessToken, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(b, &hooks); err != nil {
		return nil, err
	}
	for _, hook := range hooks {
		if strings.HasSuffix(hook.URL, fmt.Sprintf("%s%s", utils.HooksEndpointPrefix, ref.Ref(receiver))) {
			result = &hook
		}
	}
	return result, nil
}

func getFromGitlab(gitlabAccessToken string, url string) (*http.Response, error) {
	return doRequestToGitlab(http.MethodGet, url, gitlabAccessToken, nil)
}

func doRequestToGitlab(method string, url string, gitlabAccessToken string, opt interface{}) (*http.Response, error) {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: 15 * time.Second,
	}
	//set to max 100 per page to reduce query time
	if method == http.MethodGet {
		q := req.URL.Query()
		q.Set("per_page", maxPerPage)
		req.URL.RawQuery = q.Encode()
	}
	if opt != nil {
		q := req.URL.Query()
		optq, err := query.Values(opt)
		if err != nil {
			return nil, err
		}
		for k, v := range optq {
			q[k] = v
		}
		req.URL.RawQuery = q.Encode()
	}
	if gitlabAccessToken != "" {
		req.Header.Add("Authorization", "Bearer "+gitlabAccessToken)
	}
	req.Header.Add("Accept", "application/json")
	req.Header.Add("user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_10_5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/51.0.2704.103 Safari/537.36)")
	resp, err := client.Do(req)
	if err != nil {
		return resp, err
	}
	// Check the status code
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		var body bytes.Buffer
		io.Copy(&body, resp.Body)
		return resp, httperror.NewAPIErrorLong(resp.StatusCode, "", body.String())
	}

	return resp, nil
}

func getUserRepoFromURL(repoURL string) (string, string, error) {
	reg := regexp.MustCompile(".*/([^/]*?)/([^/]*?).git")
	match := reg.FindStringSubmatch(repoURL)
	if len(match) != 3 {
		return "", "", fmt.Errorf("error getting user/repo from gitrepoUrl:%v", repoURL)
	}
	return match[1], match[2], nil
}

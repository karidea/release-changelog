package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/confluentinc/confluent-kafka-go/kafka"
	"github.com/machinebox/graphql"
)

var PullRequestQuery = `
query($owner: String!, $repo: String!, $pr: Int!) {
  repository(owner: $owner, name: $repo) {
    name
    pullRequest(number: $pr) {
      title baseRefName author { login } baseRefOid headRefOid createdAt
      commits(first: 50) {
        nodes {
          commit {
            messageHeadline
            abbreviatedOid
            author { user { login } }
            associatedPullRequests(first: 1) {
              nodes {
                number
              }
            }
          }
        }
      }
    }
  }
}
`

var NpmPackageJsonQuery = `
query($owner: String!, $repo: String!) {
  repository(owner: $owner, name: $repo) {
    object(expression: "master:package.json") {
      ... on Blob {
        text
      }
    }
  }
}
`

type User struct {
	Login string
}

type Author struct {
	User User
}

type PullRequestAuthor struct {
	Login string
}

type AssociatedPullRequest struct {
	Number int
}

type AssociatedPullRequests struct {
	Nodes []AssociatedPullRequest `json:"nodes"`
}

type Commit struct {
	MessageHeadline        string                 `json:"messageHeadline"`
	AbbreviatedOid         string                 `json:"abbreviatedOid"`
	Author                 Author                 `json:"author"`
	AssociatedPullRequests AssociatedPullRequests `json:"associatedPullRequests"`
}

type CommitNodes struct {
	Commit Commit
}

type Commits struct {
	Nodes []CommitNodes
}

type PullRequest struct {
	Title       string            `json:"title"`
	CreatedAt   string            `json:"createdAt"`
	BaseRefName string            `json:"baseRefName"`
	HeadRefOid  string            `json:"headRefOid"`
	Author      PullRequestAuthor `json:"author"`
	Commits     Commits           `json:"commits"`
}

type Object struct {
	Text string `json:"text"`
}

type Repository struct {
	Name        string      `json:"name"`
	PullRequest PullRequest `json:"pullRequest"`
	Object      Object      `json:"object"`
}

type QueryResponse struct {
	Repository Repository `json:"repository"`
}

type Release struct {
	TagName         string `json:"tag_name"`
	TargetCommitish string `json:"target_commitish"`
	Name            string `json:"name"`
	Body            string `json:"body"`
}

type PR struct {
	Number int `json:"number"`
}

type PackageJson struct {
	Name string
}

type DistTags struct {
	Latest string
}

type RegistryResponse struct {
	DistTags DistTags `json:"dist-tags"`
}

var owner string
var repo string
var registry string
var tag string
var targetCommitish string
var pr int
var commit string
var dryRun bool
var kafkaTopic string

func init() {
	flag.StringVar(&repo, "repo", "", "Specify single Github repo to check (required)")
	flag.StringVar(&owner, "owner", "", "Specify Github owner to check (required)")
	flag.StringVar(&registry, "registry", "", "Specify npm registry (required)")
	flag.StringVar(&tag, "tag", "", "Specify release tag name (e.g. v1.0.2)")
	flag.StringVar(&targetCommitish, "targetRef", "", "Specify target ref oid to tag")
	flag.IntVar(&pr, "pr", 0, "List a PRs commits")
	flag.StringVar(&commit, "commit", "master", "Specify commit ref oid to base everything off of (default: master)")
	flag.BoolVar(&dryRun, "dry-run", false, "Show what the release would look like w/o publishing")
	flag.StringVar(&kafkaTopic, "kafka-topic", "", "Specify kafka topic to subscribe to")
}

func main() {
	flag.Parse()

	if len(repo) == 0 {
		flag.Usage()
		log.Fatal("repo is a required parameter")
	}

	if len(owner) == 0 {
		flag.Usage()
		log.Fatal("owner is a required parameter")
	}

	if len(registry) == 0 {
		flag.Usage()
		log.Fatal("registry is a required parameter")
	}

	githubToken := os.Getenv("TOKEN")
	bootstrapServers := os.Getenv("KAFKA_BOOTSTRAP_SERVERS")

	if len(bootstrapServers) > 0 && len(kafkaTopic) > 0 {
		err := subscribeToKafkaForRepoMessage(bootstrapServers, kafkaTopic, repo)
		if err != nil {
			log.Fatal(err)
		}
	}

	if len(tag) == 0 && len(repo) != 0 {
		name, err := getNpmPackageName(githubToken, owner, repo)
		if err != nil {
			log.Fatal(err)
		}

		version, err := getLatestVersion(registry, name)
		if err != nil {
			log.Fatal(err)
		}
		tag = "v" + version

	}

	var err error
	if pr == 0 && len(repo) != 0 {
		pr, err = getPullRequestNumber(githubToken, owner, repo, commit)
		if err != nil {
			log.Fatal(err)
		}
	}

	if pr > 0 {
		repository, err := getRepositoryPullRequest(githubToken, owner, repo, pr)
		if err != nil {
			log.Fatal(err)
		}

		var output string
		for _, node := range repository.PullRequest.Commits.Nodes {
			output += "* " + node.Commit.MessageHeadline + " @" + node.Commit.Author.User.Login + "\n"
		}

		if tag == "" {
			log.Fatal("Need to provide tag to publish release")
		}

		if targetCommitish == "" {
			targetCommitish = repository.PullRequest.BaseRefName
		}

		release := Release{TagName: tag, TargetCommitish: targetCommitish, Name: tag, Body: output}

		if !dryRun {
			err = publishRelease(githubToken, owner, repo, release)
			if err != nil {
				log.Fatal(err)
			}
		}

		fmt.Println(owner + "/" + repo + " - " + targetCommitish + ":" + tag)
		fmt.Print(output)

		os.Exit(0)
	}
}

func subscribeToKafkaForRepoMessage(bootstrapServers, kafkaTopic, repo string) (error) {
	c, err := kafka.NewConsumer(&kafka.ConfigMap{
		"bootstrap.servers": bootstrapServers,
		"group.id":          "release-changelog",
		"auto.offset.reset": "latest",
	})

	if err != nil {
		return err
	}

	c.Subscribe(kafkaTopic, nil)

	fmt.Println("Subscribed to " + kafkaTopic + "and waiting for message")
	for {
		msg, err := c.ReadMessage(-1)
		if err == nil {
			if strings.Contains(string(msg.Value), repo) {
				fmt.Printf("\n%q\n", string(msg.Value))
				fmt.Printf("%s has been published\n", repo)
				break
			}
		}
	}

	c.Unsubscribe()
	c.Close()
	return nil
}

func getLatestVersion(registry, name string) (string, error) {
	url := registry + "/" + name
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			}}}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var registryResponse RegistryResponse
	err = json.Unmarshal(body, &registryResponse)
	if err != nil {
		return "", err
	}

	return registryResponse.DistTags.Latest, nil
}

func getNpmPackageName(githubToken, owner, repo string) (string, error) {
	client := *graphql.NewClient(
		"https://api.github.com/graphql",
		graphql.WithHTTPClient(&http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				}}}))

	request := graphql.NewRequest(NpmPackageJsonQuery)
	request.Var("owner", owner)
	request.Var("repo", repo)
	request.Header.Add("Authorization", "bearer "+githubToken)

	ctx := context.Background()

	var respData QueryResponse
	if err := client.Run(ctx, request, &respData); err != nil {
		return "", err
	}

	var packageJson PackageJson
	if err := json.Unmarshal([]byte(respData.Repository.Object.Text), &packageJson); err != nil {
		return "", err
	}
	return packageJson.Name, nil
}

func getPullRequestNumber(githubToken, owner, repo, commmit string) (int, error) {
	pr := 0
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			}}}
	url := "https://api.github.com/repos/" + owner + "/" + repo + "/commits/" + commit + "/pulls"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Add("Accept", "application/vnd.github.v3+json")
	req.Header.Add("Authorization", "bearer "+githubToken)

	resp, err := client.Do(req)
	if err != nil {
		return pr, err
	}

	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return pr, err
	}

	var PRs []PR
	err = json.Unmarshal(body, &PRs)
	if err != nil {
		return pr, err
	}

	if len(PRs) > 0 {
		pr = PRs[0].Number
	}

	return pr, nil
}

func getRepositoryPullRequest(githubToken, owner, repo string, pr int) (Repository, error) {
	client := *graphql.NewClient(
		"https://api.github.com/graphql",
		graphql.WithHTTPClient(&http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				}}}))

	request := graphql.NewRequest(PullRequestQuery)
	request.Var("owner", owner)
	request.Var("repo", repo)
	request.Var("pr", pr)
	request.Header.Add("Authorization", "bearer "+githubToken)

	ctx := context.Background()

	var respData QueryResponse
	if err := client.Run(ctx, request, &respData); err != nil {
		return Repository{}, err
	}

	return respData.Repository, nil
}

func publishRelease(githubToken, owner, repo string, release Release) error {
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			}}}
	url := "https://api.github.com/repos/" + owner + "/" + repo + "/releases"
	requestBody, err := json.Marshal(release)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(requestBody))
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Add("Accept", "application/vnd.github.v3+json")
	req.Header.Add("Authorization", "bearer "+githubToken)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	return nil
}

func Exists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

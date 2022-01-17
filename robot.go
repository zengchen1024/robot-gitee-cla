package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"

	"github.com/opensourceways/community-robot-lib/config"
	"github.com/opensourceways/community-robot-lib/robot-gitee-framework"
	"github.com/opensourceways/community-robot-lib/utils"
	sdk "github.com/opensourceways/go-gitee/gitee"
	"github.com/sirupsen/logrus"
)

const (
	botName        = "cla"
	maxLengthOfSHA = 8
)

var checkCLARe = regexp.MustCompile(`(?mi)^/check-cla\s*$`)

type iClient interface {
	AddPRLabel(owner, repo string, number int32, label string) error
	RemovePRLabel(org, repo string, number int32, label string) error
	CreatePRComment(org, repo string, number int32, comment string) error
	DeletePRComment(org, repo string, ID int32) error
	GetPRCommits(org, repo string, number int32) ([]sdk.PullRequestCommits, error)
	ListPRComments(org, repo string, number int32) ([]sdk.PullRequestComments, error)
}

func newRobot(cli iClient) *robot {
	return &robot{cli: cli}
}

type robot struct {
	cli iClient
}

func (bot *robot) NewConfig() config.Config {
	return &configuration{}
}

func (bot *robot) getConfig(cfg config.Config, org, repo string) (*botConfig, error) {
	c, ok := cfg.(*configuration)
	if !ok {
		return nil, fmt.Errorf("can't convert to configuration")
	}

	if bc := c.configFor(org, repo); bc != nil {
		return bc, nil
	}

	return nil, fmt.Errorf("no config for this repo:%s/%s", org, repo)
}

func (bot *robot) RegisterEventHandler(f framework.HandlerRegitster) {
	f.RegisterPullRequestHandler(bot.handlePREvent)
	f.RegisterNoteEventHandler(bot.handleNoteEvent)
}

func (bot *robot) handlePREvent(e *sdk.PullRequestEvent, c config.Config, log *logrus.Entry) error {
	if e.GetPullRequest().GetState() != "open" {
		return nil
	}

	if v := sdk.GetPullRequestAction(e); v != sdk.PRActionOpened && v != sdk.PRActionChangedSourceBranch {
		return nil
	}

	org, repo := e.GetOrgRepo()

	cfg, err := bot.getConfig(c, org, repo)
	if err != nil {
		return err
	}

	return bot.handle(org, repo, e.GetPullRequest(), cfg, false, log)
}

func (bot *robot) handleNoteEvent(e *sdk.NoteEvent, c config.Config, log *logrus.Entry) error {
	if !e.IsCreatingCommentEvent() || !e.IsPullRequest() {
		return nil
	}

	// Only consider "/check-cla" comments.
	if !checkCLARe.MatchString(e.GetComment().GetBody()) {
		return nil
	}

	org, repo := e.GetOrgRepo()

	cfg, err := bot.getConfig(c, org, repo)
	if err != nil {
		return err
	}

	return bot.handle(org, repo, e.GetPullRequest(), cfg, true, log)
}

func (bot *robot) handle(
	org, repo string,
	pr *sdk.PullRequestHook,
	cfg *botConfig,
	notifyAuthorIfSigned bool,
	log *logrus.Entry,
) error {
	prNumber := pr.GetNumber()

	unsigned, err := bot.getPRCommitsAbout(org, repo, prNumber, cfg)
	if err != nil {
		return err
	}

	labels := pr.LabelsToSet()
	hasCLAYes := labels.Has(cfg.CLALabelYes)
	hasCLANo := labels.Has(cfg.CLALabelNo)

	deleteSignGuide(org, repo, prNumber, bot.cli)

	if len(unsigned) == 0 {
		if hasCLANo {
			if err := bot.cli.RemovePRLabel(org, repo, prNumber, cfg.CLALabelNo); err != nil {
				log.WithError(err).Warningf("Could not remove %s label.", cfg.CLALabelNo)
			}
		}

		if !hasCLAYes {
			if err := bot.cli.AddPRLabel(org, repo, prNumber, cfg.CLALabelYes); err != nil {
				log.WithError(err).Warningf("Could not add %s label.", cfg.CLALabelYes)
			}

			if notifyAuthorIfSigned {
				return bot.cli.CreatePRComment(
					org, repo, prNumber,
					alreadySigned(pr.GetUser().GetLogin()),
				)
			}
		}

		return nil
	}

	if hasCLAYes {
		if err := bot.cli.RemovePRLabel(org, repo, prNumber, cfg.CLALabelYes); err != nil {
			log.WithError(err).Warningf("Could not remove %s label.", cfg.CLALabelYes)
		}
	}

	if !hasCLANo {
		if err := bot.cli.AddPRLabel(org, repo, prNumber, cfg.CLALabelNo); err != nil {
			log.WithError(err).Warningf("Could not add %s label.", cfg.CLALabelNo)
		}
	}

	return bot.cli.CreatePRComment(
		org, repo, prNumber,
		signGuide(cfg.SignURL, generateUnSignComment(unsigned), cfg.FAQURL),
	)
}

func (bot *robot) getPRCommitsAbout(
	org, repo string,
	number int32,
	cfg *botConfig,
) ([]*sdk.PullRequestCommits, error) {
	commits, err := bot.cli.GetPRCommits(org, repo, number)
	if err != nil {
		return nil, err
	}

	if len(commits) == 0 {
		return nil, fmt.Errorf("commits is empty, cla cannot be checked")
	}

	authorEmailOfCommit := func(c *sdk.PullRequestCommits) string {
		return getAuthorOfCommit(c, cfg.CheckByCommitter, cfg.LitePRCommitter.isLitePR)
	}

	result := map[string]bool{}
	unsigned := make([]*sdk.PullRequestCommits, 0, len(commits))
	for i := range commits {
		c := &commits[i]

		email := strings.Trim(authorEmailOfCommit(c), " ")
		if !utils.IsValidEmail(email) {
			unsigned = append(unsigned, c)
			continue
		}

		if v, ok := result[email]; ok {
			if !v {
				unsigned = append(unsigned, c)
			}
			continue
		}

		b, err := isSigned(email, cfg.CheckURL)
		if err != nil {
			return nil, err
		}
		result[email] = b
		if !b {
			unsigned = append(unsigned, c)
		}
	}

	return unsigned, nil
}

func getAuthorOfCommit(
	c *sdk.PullRequestCommits,
	byCommitter bool,
	isLitePR func(email string, name string) bool,
) string {
	if c == nil || c.Commit == nil {
		return ""
	}

	commit := c.Commit

	if byCommitter {
		committer := commit.Committer
		if committer != nil && !isLitePR(committer.Email, committer.Name) {
			return committer.Email
		}
	}

	if commit.Author == nil {
		return ""
	}

	return commit.Author.Email
}

func isSigned(email, url string) (bool, error) {
	endpoint := fmt.Sprintf("%s?email=%s", url, email)

	resp, err := http.Get(endpoint)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	rb, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, fmt.Errorf("response has status %q and body %q", resp.Status, string(rb))
	}

	type signingInfo struct {
		Signed bool `json:"signed"`
	}
	var v struct {
		Data signingInfo `json:"data"`
	}

	if err := json.Unmarshal(rb, &v); err != nil {
		return false, fmt.Errorf("unmarshal failed: %s", err.Error())
	}

	return v.Data.Signed, nil
}

func deleteSignGuide(org string, repo string, number int32, c iClient) {
	v, err := c.ListPRComments(org, repo, number)
	if err != nil {
		return
	}

	prefix := signGuideTitle()
	prefixOld := "Thanks for your pull request. Before we can look at your pull request, you'll need to sign a Contributor License Agreement (CLA)."
	f := func(s string) bool {
		return strings.HasPrefix(s, prefix) || strings.HasPrefix(s, prefixOld)
	}

	for i := range v {
		if item := &v[i]; f(item.Body) {
			_ = c.DeletePRComment(org, repo, item.Id)
		}
	}
}

func signGuideTitle() string {
	return "Thanks for your pull request.\n\nThe authors of the following commits have not signed the Contributor License Agreement (CLA):"
}

func signGuide(signURL, cInfo, faq string) string {
	s := `%s

%s

Please check the [**FAQs**](%s) first.
You can click [**here**](%s) to sign the CLA. After signing the CLA, you must comment "/check-cla" to check the CLA status again.`

	return fmt.Sprintf(s, signGuideTitle(), cInfo, faq, signURL)
}

func alreadySigned(user string) string {
	s := `***@%s***, thanks for your pull request. All authors of the commits have signed the CLA. :wave: `
	return fmt.Sprintf(s, user)
}

func generateUnSignComment(commits []*sdk.PullRequestCommits) string {
	if len(commits) == 0 {
		return ""
	}

	cs := make([]string, 0, len(commits))
	for _, c := range commits {
		msg := ""
		if c.Commit != nil {
			msg = c.Commit.Message
		}

		sha := c.Sha
		if len(sha) > maxLengthOfSHA {
			sha = sha[:maxLengthOfSHA]
		}

		cs = append(cs, fmt.Sprintf("**%s** | %s", sha, msg))
	}

	return strings.Join(cs, "\n")
}

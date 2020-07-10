package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver"
	resource "github.com/concourse/registry-image-resource"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/simonshyu/notary-gcr/pkg/gcr"
	"github.com/sirupsen/logrus"
)

type Out struct {
	stdin  io.Reader
	stderr io.Writer
	stdout io.Writer
	args   []string
}

func NewOut(
	stdin io.Reader,
	stderr io.Writer,
	stdout io.Writer,
	args []string,
) *Out {
	return &Out{
		stdin:  stdin,
		stderr: stderr,
		stdout: stdout,
		args:   args,
	}
}

func (o *Out) Execute() error {
	setupLogging(o.stderr)

	var req resource.OutRequest
	decoder := json.NewDecoder(o.stdin)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&req)
	if err != nil {
		return fmt.Errorf("invalid payload: %s", err)
	}

	if req.Source.Debug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	if len(o.args) < 2 {
		return fmt.Errorf("destination path not specified")
	}

	src := o.args[1]

	if req.Source.AwsAccessKeyId != "" && req.Source.AwsSecretAccessKey != "" && req.Source.AwsRegion != "" {
		if !req.Source.AuthenticateToECR() {
			return fmt.Errorf("cannot authenticate with ECR")
		}
	}

	tagsToPush := []name.Tag{}

	repo, err := name.NewRepository(req.Source.Repository)
	if err != nil {
		return fmt.Errorf("could not resolve repository: %w", err)
	}

	if req.Source.Tag != "" {
		tagsToPush = append(tagsToPush, repo.Tag(req.Source.Tag.String()))
	}

	if req.Params.Version != "" {
		ver, err := semver.NewVersion(req.Params.Version)
		if err != nil {
			if err == semver.ErrInvalidSemVer {
				return fmt.Errorf("invalid semantic version: %q", req.Params.Version)
			}

			return fmt.Errorf("failed to parse version: %w", err)
		}

		// vito: subtle gotcha here - if someone passes the version as v1.2.3, the
		// 'v' will be stripped, as *semver.Version parses it but does not preserve
		// it in .String().
		//
		// we could call .Original(), of course, but it seems common practice to
		// *not* have the v prefix in Docker image tags, so it might be better to
		// just enforce it until someone complains enough; it seems more likely to
		// be an accident than a legacy practice that must be preserved.
		//
		// if that's the person reading this: sorry! PR welcome! (maybe we should
		// add tag_prefix:?)
		tag := ver.String()
		if req.Source.Variant != "" {
			tag += "-" + req.Source.Variant
		}

		tagsToPush = append(tagsToPush, repo.Tag(tag))

		if req.Params.BumpAliases && ver.Prerelease() == "" {
			auth := &authn.Basic{
				Username: req.Source.Username,
				Password: req.Source.Password,
			}

			imageOpts := []remote.Option{}

			if auth.Username != "" && auth.Password != "" {
				imageOpts = append(imageOpts, remote.WithAuth(auth))
			}

			versions, err := remote.List(repo, imageOpts...)
			if err != nil {
				return fmt.Errorf("list repository tags: %w", err)
			}

			bumpLatest := true
			bumpMajor := true
			bumpMinor := true
			for _, v := range versions {
				versionStr := v
				if req.Source.Variant != "" {
					versionStr = strings.TrimSuffix(versionStr, "-"+req.Source.Variant)
				}

				remoteVer, err := semver.NewVersion(versionStr)
				if err != nil {
					continue
				}

				// don't compare to prereleases or other variants
				if remoteVer.Prerelease() != "" {
					continue
				}

				if remoteVer.GreaterThan(ver) {
					bumpLatest = false
				}

				if remoteVer.Major() == ver.Major() && remoteVer.Minor() > ver.Minor() {
					bumpMajor = false
				}

				if remoteVer.Major() == ver.Major() && remoteVer.Minor() == ver.Minor() && remoteVer.Patch() > ver.Patch() {
					bumpMinor = false
					bumpMajor = false
				}
			}

			if bumpLatest {
				latestTag := "latest"
				if req.Source.Variant != "" {
					latestTag = req.Source.Variant
				}

				tagsToPush = append(tagsToPush, repo.Tag(latestTag))
			}

			if bumpMajor {
				tagName := fmt.Sprintf("%d", ver.Major())
				if req.Source.Variant != "" {
					tagName += "-" + req.Source.Variant
				}

				tagsToPush = append(tagsToPush, repo.Tag(tagName))
			}

			if bumpMinor {
				tagName := fmt.Sprintf("%d.%d", ver.Major(), ver.Minor())
				if req.Source.Variant != "" {
					tagName += "-" + req.Source.Variant
				}

				tagsToPush = append(tagsToPush, repo.Tag(tagName))
			}
		}
	}

	additionalTags, err := req.Params.ParseAdditionalTags(src)
	if err != nil {
		return fmt.Errorf("could not parse additional tags: %w", err)
	}

	for _, tagName := range additionalTags {
		tag, err := name.NewTag(fmt.Sprintf("%s:%s", req.Source.Repository, tagName))
		if err != nil {
			return fmt.Errorf("could not resolve repository/tag reference: %w", err)
		}

		tagsToPush = append(tagsToPush, tag)
	}

	if len(tagsToPush) == 0 {
		return fmt.Errorf("no tag specified - need either 'version:' in params or 'tag:' in source")
	}

	imagePath := filepath.Join(src, req.Params.Image)
	matches, err := filepath.Glob(imagePath)
	if err != nil {
		return fmt.Errorf("failed to glob path '%s': %w", req.Params.Image, err)
	}
	if len(matches) == 0 {
		return fmt.Errorf("no files match glob '%s'", req.Params.Image)
	}
	if len(matches) > 1 {
		return fmt.Errorf("too many files match glob '%s': %v", req.Params.Image, matches)
	}

	img, err := tarball.ImageFromPath(matches[0], nil)
	if err != nil {
		return fmt.Errorf("could not load image from path '%s': %w", req.Params.Image, err)
	}

	digest, err := img.Digest()
	if err != nil {
		return fmt.Errorf("failed to get image digest: %w", err)
	}

	err = resource.RetryOnRateLimit(func() error {
		return put(req, img, tagsToPush)
	})
	if err != nil {
		return fmt.Errorf("pushing image failed: %w", err)
	}

	pushedTags := []string{}
	for _, tag := range tagsToPush {
		pushedTags = append(pushedTags, tag.TagStr())
	}

	err = json.NewEncoder(os.Stdout).Encode(resource.OutResponse{
		Version: resource.Version{
			Tag:    tagsToPush[0].TagStr(),
			Digest: digest.String(),
		},
		Metadata: append(req.Source.Metadata(), resource.MetadataField{
			Name:  "tags",
			Value: strings.Join(pushedTags, " "),
		}),
	})
	if err != nil {
		return fmt.Errorf("could not marshal JSON: %s", err)
	}

	return nil
}

func put(req resource.OutRequest, img v1.Image, tags []name.Tag) error {
	auth := &authn.Basic{
		Username: req.Source.Username,
		Password: req.Source.Password,
	}

	var notaryConfigDir string
	var err error
	if req.Source.ContentTrust != nil {
		notaryConfigDir, err = req.Source.ContentTrust.PrepareConfigDir()
		if err != nil {
			return fmt.Errorf("prepare notary-config-dir: %w", err)
		}
	}

	for _, tag := range tags {
		logrus.Infof("pushing to tag %s", tag.Identifier())

		err = remote.Write(tag, img, remote.WithAuth(auth))
		if err != nil {
			return fmt.Errorf("tag image: %w", err)
		}

		logrus.Info("pushed")

		if notaryConfigDir != "" {
			trustedRepo, err := gcr.NewTrustedGcrRepository(notaryConfigDir, tag, auth)
			if err != nil {
				return fmt.Errorf("create TrustedGcrRepository: %w", err)
			}

			logrus.Info("signing image")

			err = trustedRepo.SignImage(img)
			if err != nil {
				logrus.Errorf("failed to sign image: %s", err)
			}
		}
	}

	return nil
}

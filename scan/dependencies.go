// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package scan

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strings"

	"github.com/Azure/acr-builder/pkg/image"
	"github.com/Azure/acr-builder/util"
	"github.com/docker/distribution/reference"
	"github.com/pkg/errors"
)

const (
	dockerfileComment = "#"
	defaultDockerfile = "Dockerfile"
)

var (
	utf8BOM = []byte{0xEF, 0xBB, 0xBF}
)

// ScanForDependencies scans for base image dependencies.
func (s *Scanner) ScanForDependencies(context string, workingDir string, dockerfile string, buildArgs []string, pushTo []string) (deps []*image.Dependencies, err error) {
	dockerfilePath := createDockerfilePath(context, workingDir, dockerfile)
	file, err := os.Open(dockerfilePath)
	if err != nil {
		return deps, fmt.Errorf("error opening dockerfile: %s, error: %v", dockerfilePath, err)
	}
	defer func() { _ = file.Close() }()

	runtime, buildtime, err := resolveDockerfileDependencies(file, buildArgs)
	if err != nil {
		return deps, err
	}

	// Even though there's nothing to push to, we always invoke NewImageDependencies
	// TODO: refactor this in the future to take in the full list as opposed to individual
	// images.
	var currDep *image.Dependencies
	if len(pushTo) == 0 {
		currDep, err = s.NewImageDependencies("", runtime, buildtime)
		if err != nil {
			return nil, err
		}
		deps = append(deps, currDep)
	}

	for _, imageName := range pushTo {
		currDep, err = s.NewImageDependencies(imageName, runtime, buildtime)
		if err != nil {
			return nil, err
		}
		deps = append(deps, currDep)
	}

	return deps, err
}

// NewImageDependencies creates Dependencies with no references registered
func (s *Scanner) NewImageDependencies(img string, runtime string, buildtimes []string) (*image.Dependencies, error) {
	var dependencies *image.Dependencies
	if len(img) > 0 {
		imageReference, err := NewImageReference(NormalizeImageTag(img))
		if err != nil {
			return nil, err
		}
		dependencies = &image.Dependencies{
			Image: imageReference,
		}
	} else {
		// we allow build without pushing image to registry so the image can be empty
		dependencies = &image.Dependencies{
			Image: nil,
		}
	}

	runtimeDep, err := NewImageReference(NormalizeImageTag(runtime))
	if err != nil {
		return nil, err
	}
	dependencies.Runtime = runtimeDep

	dict := map[string]bool{}
	for _, buildtime := range buildtimes {
		bt := NormalizeImageTag(buildtime)

		// If the image is prefixed with "library/", remove it for comparisons.
		// "library/" will be added again during image reference generation.
		// This prevents duplicate dependencies when reading "library/golang" and
		// "golang" from the Dockerfile.
		bt = strings.TrimPrefix(bt, "library/")

		// If we've already processed the tag after normalization, skip dependency
		// generation. I.e., they specify "golang" and "golang:latest"
		if dict[bt] {
			continue
		}

		dict[bt] = true

		buildtimeDep, err := NewImageReference(bt)
		if err != nil {
			return nil, err
		}
		dependencies.Buildtime = append(dependencies.Buildtime, buildtimeDep)
	}
	return dependencies, nil
}

// NormalizeImageTag adds "latest" to the image if the specified image
// has no tag and it's not referenced by digest.
func NormalizeImageTag(img string) string {
	if !strings.Contains(img, "@") && !strings.Contains(img, ":") {
		return fmt.Sprintf("%s:latest", img)
	}
	return img
}

// NewImageReference parses a path of a image and creates a ImageReference object
func NewImageReference(imagePath string) (*image.Reference, error) {
	ref, err := reference.Parse(imagePath)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to parse image reference, ensure tags have a valid format: %s", imagePath)
	}
	result := &image.Reference{
		Reference: ref.String(),
	}

	if named, ok := ref.(reference.Named); ok {
		result.Registry = reference.Domain(named)

		if strings.Contains(result.Registry, ".") {
			// The domain is the registry, eg, registryname.azurecr.io
			result.Repository = reference.Path(named)
		} else {
			// DockerHub
			if result.Registry == "" {
				result.Registry = DockerHubRegistry
				result.Repository = strings.Join([]string{"library", reference.Path(named)}, "/")
			} else {
				// The domain is the DockerHub user name
				result.Registry = DockerHubRegistry
				result.Repository = strings.Join([]string{reference.Domain(named), reference.Path(named)}, "/")
			}
		}
	}
	if tagged, ok := ref.(reference.Tagged); ok {
		result.Tag = tagged.Tag()
	}
	return result, nil
}

// resolveDockerfileDependencies resolves dependencies given an io.Reader for a Dockerfile.
func resolveDockerfileDependencies(r io.Reader, buildArgs []string) (origin string, buildtimeDependencies []string, err error) {
	scanner := bufio.NewScanner(r)
	context, err := parseBuildArgs(buildArgs)
	if err != nil {
		return "", nil, err
	}
	originLookup := map[string]string{} // given an alias, look up its origin
	allOrigins := map[string]bool{}     // set of all origins
	firstLine := true
	for scanner.Scan() {
		var line string
		// Trim UTF-8 BOM if necessary.
		if firstLine {
			scannedBytes := scanner.Bytes()
			scannedBytes = bytes.TrimPrefix(scannedBytes, utf8BOM)
			line = string(scannedBytes)
			firstLine = false
		} else {
			line = scanner.Text()
		}

		line = strings.TrimSpace(line)
		// Skip comments.
		if line == "" || strings.HasPrefix(line, dockerfileComment) {
			continue
		}

		tokens := strings.Fields(line)
		if len(tokens) > 0 {
			switch strings.ToUpper(tokens[0]) {
			case "FROM":
				if len(tokens) < 2 {
					return "", nil, fmt.Errorf("unable to understand line %s", line)
				}
				var imageToken = os.Expand(tokens[1], func(key string) string {
					return context[key]
				})
				var found bool
				origin, found = originLookup[imageToken]
				if !found {
					allOrigins[imageToken] = true
					origin = imageToken
				}

				if len(tokens) > 2 {
					if len(tokens) < 4 || !strings.EqualFold(tokens[2], "as") {
						return "", nil, fmt.Errorf("unable to understand line %s", line)
					}
					// alias cannot contain variables it seems. So we don't call context.Expand on it
					alias := tokens[3]
					originLookup[alias] = origin
					// Just ignore the rest of the tokens...
					if len(tokens) > 4 {
						log.Printf("Ignoring chunks from FROM clause: %v\n", tokens[4:])
					}
				}
			case "ARG":
				if len(tokens) < 2 {
					return "", nil, fmt.Errorf("dockerfile syntax requires ARG directive to have exactly 1 argument. LINE: %s", line)
				}
				if strings.Contains(tokens[1], "=") {
					varName, varValue, err := parseAssignment(tokens[1])
					if err != nil {
						return "", nil, fmt.Errorf("unable to parse assignment %s, error: %s", tokens[1], err)
					}
					// This line matches docker's behavior here
					// 1. If build arg is passed in, the value will not override
					// 2. It is actually allowed for same ARG to be specified more than once in a Dockerfile
					//    However the subsequent value would be ignored instead of overriding the previous
					if _, found := context[varName]; !found {
						context[varName] = varValue
					}
				}
			}
		}
	}

	if len(allOrigins) == 0 {
		return "", nil, errors.New("unexpected dockerfile format")
	}

	// note that origin variable now points to the runtime origin
	for terminal := range allOrigins {
		if terminal != origin {
			buildtimeDependencies = append(buildtimeDependencies, terminal)
		}
	}

	return origin, buildtimeDependencies, nil
}

func parseBuildArgs(args []string) (map[string]string, error) {
	result := map[string]string{}
	for _, assignment := range args {
		name, value, err := parseAssignment(assignment)
		if err != nil {
			return nil, err
		}
		result[name] = value
	}
	return result, nil
}

func parseAssignment(in string) (name string, value string, err error) {
	values := strings.SplitN(in, "=", 2)
	if len(values) != 2 {
		return "", "", fmt.Errorf("%s cannot be split into 2 tokens with '='", in)
	}

	return values[0], removeSurroundingQuotes(values[1]), nil
}

// removeSurroundingQuotes trims double quotes, then single quotes.
func removeSurroundingQuotes(s string) string {
	s = strings.Trim(s, `"`)
	return strings.Trim(s, `'`)
}

// createDockerfilePath determines where we should look for the dockerfile depending on the
// context and working directory.
func createDockerfilePath(context string, workingDir string, dockerfile string) string {
	dockerfilePath := dockerfile
	isLocalContext := util.IsLocalContext(context)

	// For local context:
	// - If the Dockerfile wasn't specified, we default its value and check for it relative to the working directory.
	// - If the Dockerfile was specified, we simply look at the provided Dockerfile's path.
	//
	// For remote context:
	// - The Dockerfile is scoped to the cloned or downloaded location and the working directory.
	//   I.e., for https://github.com/Azure/acr-builder.git#:foo/bar, which was downloaded to a directory "build",
	//   the path must be scoped to build/foo/bar/Dockerfile
	if isLocalContext && dockerfile == "" {
		dockerfilePath = path.Clean(path.Join(workingDir, defaultDockerfile))
	} else if !isLocalContext {
		if dockerfile == "" {
			dockerfile = defaultDockerfile
		}
		dockerfilePath = path.Clean(path.Join(workingDir, dockerfile))
	}

	return dockerfilePath
}

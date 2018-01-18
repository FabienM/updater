// Package updater provides a tool to update a binary relying from a http repository (such as Nexus)
package updater

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/coreos/go-semver/semver"
)

// BuildInfo is an entry of the repository
type BuildInfo struct {
	Name    string
	File    string
	Version *semver.Version
	Os      string
	Arch    string
	MD5     string
	URL     string
}

type by func(build1, build2 BuildInfo) bool

type buildInfoSorter struct {
	builds []*BuildInfo
	by     func(build1, build2 BuildInfo) bool
}

// Matcher is a function that can be called to know if a repository entry can be considered as a valid update
type Matcher func(info *BuildInfo) bool

// Config is the public configuration object to create an Updater
type Config struct {
	// BinaryName is the base name of binary artifacts
	BinaryName string
	// TargetPath is the local path to update (default to os.Executable())
	TargetPath string
	// Fields is an ordered array describing filename structure, using field* constants
	Fields []field
	// FieldSeparator is the string separating fields in the filename
	FieldSeparator string
	// SortCriteria is the sort order that will be applied to find the latest version
	SortCriteria sortCriteria
	// Matcher is a pointer to the wanted Matcher func
	Matcher *Matcher
	// Repository is the url of the repository where updates are to be found
	Repository string
	// TmpPattern is a sprintf pattern defining the local temporary storage for downloaded files
	TmpPattern string
}

// Updater is the main object
type Updater struct {
	Config
}

type sortCriteria int

const (
	// SortSemver sorts repository entries by version number, according to semver
	SortSemver sortCriteria = iota
)

type field int

const (
	// FieldName is the binary name
	FieldName field = iota
	// FieldVersion is the binary version number
	FieldVersion
	// FieldOs is the binary target OS
	FieldOs
	// FieldArch is the binary target architecture
	FieldArch
)

var (
	sorts = map[sortCriteria]by{
		SortSemver: bySemver,
	}
)

// New creates an updater with default values if they are missing in the Config
func New(config Config) Updater {
	if config.SortCriteria == 0 {
		config.SortCriteria = SortSemver
	}
	if config.Matcher == nil {
		defaultMatcher := nameCurrentOsArchMatcher(config.BinaryName)
		config.Matcher = &defaultMatcher
	}
	if len(config.Fields) == 0 {
		config.Fields = []field{FieldName, FieldVersion, FieldOs, FieldArch}
	}
	if config.FieldSeparator == "" {
		config.FieldSeparator = "-"
	}
	if config.TmpPattern == "" {
		config.TmpPattern = string(os.PathSeparator) + "tmp" + string(os.PathSeparator) + "%s.tmp"
	}
	return Updater{
		config,
	}
}

// FindLatest returns the latest eligible build in the repository.
// It finds all anchors in a html page and try to consider them as a valid build.
// The latest build that matches, according to the matcher and the sortCriteria order is returned.
func (u Updater) FindLatest() (*BuildInfo, error) {
	buildList, err := u.fetchBuildList()
	if err != nil {
		return nil, err
	}

	if len(buildList) == 0 {
		return nil, nil
	}

	u.SortCriteria.sort(buildList)
	return buildList[len(buildList)-1], nil
}

// UpdateTo download the referenced build and move it to the target path
func (u Updater) UpdateTo(build *BuildInfo) error {
	path := u.TargetPath
	var err error
	if path == "" {
		path, err = os.Executable()
	}
	if err != nil {
		return fmt.Errorf("cannot find current executable path: %v", err)
	}
	tmpPath := fmt.Sprintf(u.TmpPattern, build.File)
	tmpFile, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0777)
	if err != nil {
		return fmt.Errorf("cannot create temporary file %s: %v", tmpPath, err)
	}
	defer func() {
		tmpFile.Close()
		os.Remove(tmpPath)
	}()
	resp, err := http.Get(build.URL)
	if err != nil {
		return fmt.Errorf("cannot download new version at %s: %v", build.URL, err)
	}
	defer resp.Body.Close()
	_, err = io.Copy(tmpFile, resp.Body)
	if err != nil {
		return err
	}

	err = os.Rename(tmpPath, path)
	if err != nil {
		return fmt.Errorf("cannot move temporary file %s to %s: %s", tmpPath, path, err)
	}

	return nil
}

// NewerThan returns true if the referenced build is newer that the given version string.
func (build *BuildInfo) NewerThan(version string) bool {
	if build.Version == nil {
		return false
	}
	return semver.New(version).LessThan(*build.Version)
}

func nameCurrentOsArchMatcher(name string) Matcher {
	return func(build *BuildInfo) bool {
		return build != nil && build.Name == name && build.Arch == runtime.GOARCH && build.Os == runtime.GOOS
	}
}

func (by sortCriteria) sort(buildList []*BuildInfo) {
	sorter := &buildInfoSorter{
		builds: buildList,
		by:     sorts[by],
	}
	sort.Sort(sorter)
}

func (u Updater) fetchBuildList() ([]*BuildInfo, error) {
	list := make([]*BuildInfo, 0)
	resp, err := http.Get(u.Repository)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile("<a [^>]*href=\"([^\"]+)\"[^>]*>([^<]+)</a>")
	matches := re.FindAllSubmatch(body, -1)
	for _, match := range matches {
		buildInfo := u.tokenizeBuild(string(match[2]))
		if buildInfo != nil && (*u.Matcher)(buildInfo) {
			buildInfo.URL = string(match[1])
			list = append(list, buildInfo)
		}
	}
	return list, nil
}

func (u Updater) tokenizeBuild(buildName string) *BuildInfo {
	split := strings.Split(strings.TrimSuffix(buildName, ".exe"), u.FieldSeparator)
	if len(split) != len(u.Fields) {
		return nil
	}

	tokens := map[field]string{}
	for key, field := range u.Fields {
		tokens[field] = split[key]
	}

	version, _ := semver.NewVersion(tokens[FieldVersion])

	return &BuildInfo{
		Name:    tokens[FieldName],
		Version: version,
		Os:      tokens[FieldOs],
		Arch:    tokens[FieldArch],
		File:    buildName,
	}
}

func bySemver(build1, build2 BuildInfo) bool {
	if build1.Version == nil {
		return true
	}
	if build2.Version == nil {
		return false
	}
	return build1.Version.LessThan(*build2.Version)
}

func (s *buildInfoSorter) Less(i, j int) bool {
	return s.by(*s.builds[i], *s.builds[j])
}

func (s *buildInfoSorter) Len() int {
	return len(s.builds)
}

func (s *buildInfoSorter) Swap(i, j int) {
	s.builds[i], s.builds[j] = s.builds[j], s.builds[i]
}

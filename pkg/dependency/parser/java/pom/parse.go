package pom

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/aquasecurity/trivy/pkg/dependency"
	"github.com/aquasecurity/trivy/pkg/dependency/parser/utils"
	ftypes "github.com/aquasecurity/trivy/pkg/fanal/types"
	"github.com/aquasecurity/trivy/pkg/log"
	xio "github.com/aquasecurity/trivy/pkg/x/io"
	"github.com/hashicorp/go-multierror"
	"github.com/samber/lo"
	"golang.org/x/net/html/charset"
	"golang.org/x/xerrors"
)

// Default Maven central URL
const defaultCentralUrl = "https://repo.maven.apache.org/maven2/"

// Ordered list of URLs to use to fetch Maven dependency metadata.
// If there is an error fetching a dependency from a URL, the next URL is used, and so on.
var mavenReleaseRepos []string

func init() {
	if url, ok := os.LookupEnv("MAVEN_CENTRAL_URL"); ok {
		// Use the default Maven central URL in case the
		mavenReleaseRepos = []string{url, defaultCentralUrl}
	} else {
		mavenReleaseRepos = []string{defaultCentralUrl}
	}
}

type options struct {
	offline             bool
	useMavenCache       bool
	mavenCacheTtl       int
	releaseRemoteRepos  []string
	snapshotRemoteRepos []string
}

type option func(*options)

func WithOffline(offline bool) option {
	return func(opts *options) {
		opts.offline = offline
	}
}

func WithUseMavenCache(useMavenCache bool) option {
	return func(opts *options) {
		opts.useMavenCache = useMavenCache
	}
}

func WithMavenCacheTtl(ttl int) option {
	return func(opts *options) {
		opts.mavenCacheTtl = ttl
	}
}

func WithReleaseRemoteRepos(repos []string) option {
	return func(opts *options) {
		opts.releaseRemoteRepos = repos
	}
}

func WithSnapshotRemoteRepos(repos []string) option {
	return func(opts *options) {
		opts.snapshotRemoteRepos = repos
	}
}

type Parser struct {
	logger              *log.Logger
	rootPath            string
	cache               pomCache
	mavenHttpCache      *mavenHttpCache
	localRepository     string
	releaseRemoteRepos  []string
	snapshotRemoteRepos []string
	offline             bool
	servers             []Server
}

func NewParser(filePath string, opts ...option) *Parser {
	var logger = log.WithPrefix("pom")

	o := &options{
		offline:            false,
		useMavenCache:      false,
		mavenCacheTtl:      720,
		releaseRemoteRepos: mavenReleaseRepos, // Maven doesn't use central repository for snapshot dependencies
	}

	logger.Debug("Creating parser", log.String("releaseRemoteRepos", strings.Join(mavenReleaseRepos, ", ")))

	for _, opt := range opts {
		opt(o)
	}

	s := readSettings()
	localRepository := s.LocalRepository
	if localRepository == "" {
		homeDir, _ := os.UserHomeDir()
		localRepository = filepath.Join(homeDir, ".m2", "repository")
	}

	var mavenHttpCache *mavenHttpCache = nil

	if o.useMavenCache {
		mavenHttpCache = newMavenHttpCache(logger, o.mavenCacheTtl)
	}

	return &Parser{
		logger:              logger,
		rootPath:            filepath.Clean(filePath),
		cache:               newPOMCache(),
		mavenHttpCache:      mavenHttpCache,
		localRepository:     localRepository,
		releaseRemoteRepos:  o.releaseRemoteRepos,
		snapshotRemoteRepos: o.snapshotRemoteRepos,
		offline:             o.offline,
		servers:             s.Servers,
	}
}

func (p *Parser) Parse(r xio.ReadSeekerAt) ([]ftypes.Package, []ftypes.Dependency, error) {
	content, err := parsePom(r)
	if err != nil {
		return nil, nil, xerrors.Errorf("failed to parse POM: %w", err)
	}

	root := &pom{
		filePath: p.rootPath,
		content:  content,
	}

	result, err := p.analyze(root, analysisOptions{lineNumber: true}, map[string]struct{}{})
	if err != nil {
		return nil, nil, xerrors.Errorf("analyze error (%s): %w", p.rootPath, err)
	}

	// Cache root POM
	p.cache.put(result.artifact, result)

	return p.parseRoot(root.artifact(), make(map[string]struct{}), map[string]struct{}{})
}

func (p *Parser) parseRoot(root artifact, uniqModules map[string]struct{}, visitedLocalPaths map[string]struct{}) ([]ftypes.Package, []ftypes.Dependency, error) {
	// Prepare a queue for dependencies
	queue := newArtifactQueue()

	// Enqueue root POM
	root.Relationship = ftypes.RelationshipRoot
	root.Module = false
	queue.enqueue(root)

	var (
		pkgs              ftypes.Packages
		deps              ftypes.Dependencies
		rootDepManagement []pomDependency
		uniqArtifacts     = make(map[string]artifact)
		uniqDeps          = make(map[string][]string)
	)

	// Iterate direct and transitive dependencies
	for !queue.IsEmpty() {
		art := queue.dequeue()

		// Modules should be handled separately so that they can have independent dependencies.
		// It means multi-module allows for duplicate dependencies.
		if art.Module {
			if _, ok := uniqModules[art.String()]; ok {
				continue
			}
			uniqModules[art.String()] = struct{}{}

			modulePkgs, moduleDeps, err := p.parseRoot(art, uniqModules, visitedLocalPaths)
			if err != nil {
				return nil, nil, err
			}

			pkgs = append(pkgs, modulePkgs...)
			if moduleDeps != nil {
				deps = append(deps, moduleDeps...)
			}
			continue
		}

		// For soft requirements, skip dependency resolution that has already been resolved.
		if uniqueArt, ok := uniqArtifacts[art.Name()]; ok {
			if !uniqueArt.Version.shouldOverride(art.Version) {
				continue
			}
			// mark artifact as Direct, if saved artifact is Direct
			// take a look `hard requirement for the specified version` test
			if uniqueArt.Relationship == ftypes.RelationshipRoot || uniqueArt.Relationship == ftypes.RelationshipDirect {
				art.Relationship = uniqueArt.Relationship
			}
			// We don't need to overwrite dependency location for hard links
			if uniqueArt.Locations != nil {
				art.Locations = uniqueArt.Locations
			}
		}

		result, err := p.resolve(art, rootDepManagement, visitedLocalPaths)
		if err != nil {
			return nil, nil, xerrors.Errorf("resolve error (%s): %w", art, err)
		}

		if art.Relationship == ftypes.RelationshipRoot {
			// Managed dependencies in the root POM affect transitive dependencies
			rootDepManagement = p.resolveDepManagement(result.properties, result.dependencyManagement, visitedLocalPaths)

			// mark its dependencies as "direct"
			result.dependencies = lo.Map(result.dependencies, func(dep artifact, _ int) artifact {
				dep.Relationship = ftypes.RelationshipDirect
				return dep
			})
		}

		// Parse, cache, and enqueue modules.
		for _, relativePath := range result.modules {
			moduleArtifact, err := p.parseModule(result.filePath, relativePath, visitedLocalPaths)
			if err != nil {
				p.logger.Debug("Unable to parse the module",
					log.FilePath(result.filePath), log.Err(err))
				continue
			}

			queue.enqueue(moduleArtifact)
		}

		// Resolve transitive dependencies later
		queue.enqueue(result.dependencies...)

		// Offline mode may be missing some fields.
		if !art.IsEmpty() {
			// Override the version
			uniqArtifacts[art.Name()] = artifact{
				Version:      art.Version,
				Licenses:     result.artifact.Licenses,
				Relationship: art.Relationship,
				Locations:    art.Locations,
			}

			// save only dependency names
			// version will be determined later
			dependsOn := lo.Map(result.dependencies, func(a artifact, _ int) string {
				return a.Name()
			})
			uniqDeps[packageID(art.Name(), art.Version.String())] = dependsOn
		}
	}

	// Convert to []ftypes.Package and []ftypes.Dependency
	for name, art := range uniqArtifacts {
		pkg := ftypes.Package{
			ID:           packageID(name, art.Version.String()),
			Name:         name,
			Version:      art.Version.String(),
			Licenses:     art.Licenses,
			Relationship: art.Relationship,
			Locations:    art.Locations,
		}
		pkgs = append(pkgs, pkg)

		// Convert dependency names into dependency IDs
		dependsOn := lo.FilterMap(uniqDeps[pkg.ID], func(dependOnName string, _ int) (string, bool) {
			ver := depVersion(dependOnName, uniqArtifacts)
			return packageID(dependOnName, ver), ver != ""
		})

		sort.Strings(dependsOn)
		if len(dependsOn) > 0 {
			deps = append(deps, ftypes.Dependency{
				ID:        pkg.ID,
				DependsOn: dependsOn,
			})
		}
	}

	sort.Sort(pkgs)
	sort.Sort(deps)

	return pkgs, deps, nil
}

// depVersion finds dependency in uniqArtifacts and return its version
func depVersion(depName string, uniqArtifacts map[string]artifact) string {
	if art, ok := uniqArtifacts[depName]; ok {
		return art.Version.String()
	}
	return ""
}

func (p *Parser) parseModule(currentPath, relativePath string, visitedLocalPaths map[string]struct{}) (artifact, error) {
	// modulePath: "root/" + "module/" => "root/module"
	p.logger.Debug("parseModule", log.String("currentPath", currentPath), log.String("relativePath", relativePath), log.String("visitedLocalPaths", fmt.Sprintf("%v", visitedLocalPaths)))
	module, err := p.openRelativePom(currentPath, relativePath)
	if err != nil {
		return artifact{}, xerrors.Errorf("unable to open the relative path: %w", err)
	}

	result, err := p.analyze(module, analysisOptions{}, visitedLocalPaths)
	if err != nil {
		return artifact{}, xerrors.Errorf("analyze error: %w", err)
	}

	moduleArtifact := module.artifact()
	moduleArtifact.Module = true // TODO: introduce RelationshipModule?

	p.cache.put(moduleArtifact, result)

	return moduleArtifact, nil
}

func (p *Parser) resolve(art artifact, rootDepManagement []pomDependency, visitedLocalPaths map[string]struct{}) (analysisResult, error) {
	p.logger.Debug("resolve", log.String("artifact", art.String()))
	// If the artifact is found in cache, it is returned.
	if result := p.cache.get(art); result != nil {
		p.logger.Debug("resolve: cache hit", log.String("artifact", art.String()))
		return *result, nil
	}

	// We can't resolve a dependency without a version.
	// So let's just keep this dependency.
	if art.Version.String() == "" {
		return analysisResult{
			artifact: art,
		}, nil
	}

	p.logger.Debug("resolve: Resolving...", log.String("group_id", art.GroupID),
		log.String("artifact_id", art.ArtifactID), log.String("version", art.Version.String()))
	pomContent, err := p.tryRepository(art.GroupID, art.ArtifactID, art.Version.String())
	if err != nil {
		p.logger.Debug("Repository error", log.Err(err))
	}
	result, err := p.analyze(pomContent, analysisOptions{
		exclusions:    art.Exclusions,
		depManagement: rootDepManagement,
	}, visitedLocalPaths)
	if err != nil {
		return analysisResult{}, xerrors.Errorf("analyze error: %w", err)
	}

	p.cache.put(art, result)
	return result, nil
}

type analysisResult struct {
	filePath             string
	artifact             artifact
	dependencies         []artifact
	dependencyManagement []pomDependency // Keep the order of dependencies in 'dependencyManagement'
	properties           map[string]string
	modules              []string
}

type analysisOptions struct {
	exclusions    map[string]struct{}
	depManagement []pomDependency // from the root POM
	lineNumber    bool            // Save line numbers
}

func (p *Parser) analyze(pom *pom, opts analysisOptions, visitedLocalPaths map[string]struct{}) (analysisResult, error) {
	if pom == nil || pom.content == nil {
		p.logger.Debug("analyze: pom is nil, skipping")
		return analysisResult{}, nil
	}

	p.logger.Debug("analyze", log.String("pom.filePath", pom.filePath), log.String("pom", pom.String()))

	if pom.filePath != "" {
		if _, seen := visitedLocalPaths[pom.filePath]; seen {
			p.logger.Warn("analyze: pom already analyzed, skipping...", log.String("pom.filePath", pom.filePath))
			return analysisResult{}, nil
		}

		visitedLocalPaths[pom.filePath] = struct{}{}
	}

	// Update remoteRepositories
	pomReleaseRemoteRepos, pomSnapshotRemoteRepos := pom.repositories(p.servers)
	p.releaseRemoteRepos = lo.Uniq(append(pomReleaseRemoteRepos, p.releaseRemoteRepos...))
	p.snapshotRemoteRepos = lo.Uniq(append(pomSnapshotRemoteRepos, p.snapshotRemoteRepos...))

	// We need to forward dependencyManagements from current and root pom to Parent,
	// to use them for dependencies in parent.
	// For better understanding see the following tests:
	// - `dependency from parent uses version from child pom depManagement`
	// - `dependency from parent uses version from root pom depManagement`
	//
	// depManagements from root pom has higher priority than depManagements from current pom.
	depManagementForParent := lo.UniqBy(append(opts.depManagement, pom.content.DependencyManagement.Dependencies.Dependency...),
		func(dep pomDependency) string {
			return dep.Name()
		})

	// Parent
	p.logger.Debug("analyze: parseParent", log.String("pom.filePath", pom.filePath), log.String("parent", pom.content.Parent.String()), log.Int("numDepManagement", len(depManagementForParent)))
	parent, err := p.parseParent(pom.filePath, pom.content.Parent, depManagementForParent, visitedLocalPaths)
	if err != nil {
		return analysisResult{}, xerrors.Errorf("parent error: %w", err)
	}

	p.logger.Debug("analyze: parseParent success")

	// Inherit values/properties from parent
	pom.inherit(parent)
	p.logger.Debug("analyze: inherit success")
	// Generate properties
	props := pom.properties()

	// dependencyManagements have the next priority:
	// 1. Managed dependencies from this POM
	// 2. Managed dependencies from parent of this POM
	depManagement := p.mergeDependencyManagements(pom.content.DependencyManagement.Dependencies.Dependency,
		parent.dependencyManagement)
	p.logger.Debug("analyze: mergeDependencyManagements success")

	// Merge dependencies. Child dependencies must be preferred than parent dependencies.
	// Parents don't have to resolve dependencies.
	deps := p.parseDependencies(pom.content.Dependencies.Dependency, props, depManagement, opts, visitedLocalPaths)
	p.logger.Debug("analyze: parseDependencies success")
	deps = p.mergeDependencies(parent.dependencies, deps, opts.exclusions)
	p.logger.Debug("analyze: mergeDependencies success")

	return analysisResult{
		filePath:             pom.filePath,
		artifact:             pom.artifact(),
		dependencies:         deps,
		dependencyManagement: depManagement,
		properties:           props,
		modules:              pom.content.Modules.Module,
	}, nil
}

func (p *Parser) mergeDependencyManagements(depManagements ...[]pomDependency) []pomDependency {
	uniq := make(map[string]struct{})
	var depManagement []pomDependency
	// The preceding argument takes precedence.
	for _, dm := range depManagements {
		for _, dep := range dm {
			if _, ok := uniq[dep.Name()]; ok {
				continue
			}
			depManagement = append(depManagement, dep)
			uniq[dep.Name()] = struct{}{}
		}
	}
	return depManagement
}

func (p *Parser) parseDependencies(deps []pomDependency, props map[string]string, depManagement []pomDependency,
	opts analysisOptions, visitedLocalPaths map[string]struct{}) []artifact {
	// Imported POMs often have no dependencies, so dependencyManagement resolution can be skipped.
	if len(deps) == 0 {
		return nil
	}

	// Resolve dependencyManagement
	p.logger.Debug("parseDependencies: resolveDepManagement...")
	depManagement = p.resolveDepManagement(props, depManagement, visitedLocalPaths)
	p.logger.Debug("parseDependencies: resolveDepManagement success")
	rootDepManagement := opts.depManagement
	var dependencies []artifact
	for _, d := range deps {
		// Resolve dependencies
		p.logger.Debug("parseDependencies: Resolving dep " + fmt.Sprintf("%s:%s:%s", d.GroupID, d.ArtifactID, d.Version))
		d = d.Resolve(props, depManagement, rootDepManagement)
		p.logger.Debug("parseDependencies: Resolve dep success")

		if (d.Scope != "" && d.Scope != "compile" && d.Scope != "runtime") || d.Optional {
			continue
		}

		dependencies = append(dependencies, d.ToArtifact(opts))
	}
	return dependencies
}

func (p *Parser) resolveDepManagement(props map[string]string, depManagement []pomDependency, visitedLocalPaths map[string]struct{}) []pomDependency {
	var newDepManagement, imports []pomDependency
	for _, dep := range depManagement {
		// cf. https://howtodoinjava.com/maven/maven-dependency-scopes/#import
		if dep.Scope == "import" {
			imports = append(imports, dep)
		} else {
			// Evaluate variables
			newDepManagement = append(newDepManagement, dep.Resolve(props, nil, nil))
		}
	}

	// Managed dependencies with a scope of "import" should be processed after other managed dependencies.
	// cf. https://maven.apache.org/guides/introduction/introduction-to-dependency-mechanism.html#importing-dependencies
	for _, imp := range imports {
		art := newArtifact(imp.GroupID, imp.ArtifactID, imp.Version, nil, props)
		result, err := p.resolve(art, nil, visitedLocalPaths)
		if err != nil {
			continue
		}

		// We need to recursively check all nested depManagements,
		// so that we don't miss dependencies on nested depManagements with `Import` scope.
		newProps := utils.MergeMaps(props, result.properties)
		result.dependencyManagement = p.resolveDepManagement(newProps, result.dependencyManagement, visitedLocalPaths)
		for k, dd := range result.dependencyManagement {
			// Evaluate variables and overwrite dependencyManagement
			result.dependencyManagement[k] = dd.Resolve(newProps, nil, nil)
		}
		newDepManagement = p.mergeDependencyManagements(newDepManagement, result.dependencyManagement)
	}
	return newDepManagement
}

func (p *Parser) mergeDependencies(parent, child []artifact, exclusions map[string]struct{}) []artifact {
	var deps []artifact
	unique := make(map[string]struct{})

	for _, d := range append(child, parent...) {
		if excludeDep(exclusions, d) {
			continue
		}
		if _, ok := unique[d.Name()]; ok {
			continue
		}
		unique[d.Name()] = struct{}{}
		deps = append(deps, d)
	}

	return deps
}

func excludeDep(exclusions map[string]struct{}, art artifact) bool {
	if _, ok := exclusions[art.Name()]; ok {
		return true
	}
	// Maven can use "*" in GroupID and ArtifactID fields to exclude dependencies
	// https://maven.apache.org/pom.html#exclusions
	for exlusion := range exclusions {
		// exclusion format - "<groupID>:<artifactID>"
		e := strings.Split(exlusion, ":")
		if (e[0] == art.GroupID || e[0] == "*") && (e[1] == art.ArtifactID || e[1] == "*") {
			return true
		}
	}
	return false
}

func (p *Parser) parseParent(currentPath string, parent pomParent, rootDepManagement []pomDependency, visitedLocalPaths map[string]struct{}) (analysisResult, error) {
	p.logger.Debug("parseParent", log.String("currentPath", currentPath), log.String("parent", parent.String()), log.Int("numRootDeps", len(rootDepManagement)))
	// Pass nil properties so that variables in <parent> are not evaluated.
	target := newArtifact(parent.GroupId, parent.ArtifactId, parent.Version, nil, nil)
	// if version is property (e.g. ${revision}) - we still need to parse this pom
	if target.IsEmpty() && !isProperty(parent.Version) {
		return analysisResult{}, nil
	}

	logger := p.logger.With("artifact", target.String())
	logger.Debug("parseParent: Start parent")
	defer logger.Debug("parseParent: Exit parent")

	// If the artifact is found in cache, it is returned.
	if result := p.cache.get(target); result != nil {
		logger.Debug("parseParent: cache hit")
		return *result, nil
	}

	logger.Debug("parseParent: retrieving parent")
	parentPOM, err := p.retrieveParent(currentPath, parent.RelativePath, target, visitedLocalPaths)
	if err != nil {
		logger.Debug("Parent POM not found", log.Err(err))
	}

	logger.Debug("parseParent: analyzing parent POM")
	result, err := p.analyze(parentPOM, analysisOptions{
		depManagement: rootDepManagement,
	}, visitedLocalPaths)
	if err != nil {
		return analysisResult{}, xerrors.Errorf("analyze error: %w", err)
	}

	p.cache.put(target, result)

	return result, nil
}

func (p *Parser) retrieveParent(currentPath, relativePath string, target artifact, visitedLocalPaths map[string]struct{}) (*pom, error) {
	p.logger.Debug("retrieveParent", log.String("currentPath", currentPath), log.String("relativePath", relativePath), log.String("target", target.String()))

	var errs error

	// Try relativePath
	if relativePath != "" {
		p.logger.Debug("retrieveParent: tryRelativePath, path=" + relativePath)
		pom, err := p.tryRelativePath(target, currentPath, relativePath, visitedLocalPaths)
		if err != nil {
			p.logger.Debug("retrieveParent: tryRelativePath error", log.Err(err))
			errs = multierror.Append(errs, err)
		} else {
			p.logger.Debug("retrieveParent: resolved pom tryRelativePath")
			return pom, nil
		}
	}

	// If not found, search the parent directory
	p.logger.Debug("retrieveParent: tryRelativePath, path=../pom.xml")
	pom, err := p.tryRelativePath(target, currentPath, "../pom.xml", visitedLocalPaths)
	if err != nil {
		p.logger.Debug("retrieveParent: tryRelativePath error", log.Err(err))
		errs = multierror.Append(errs, err)
	} else {
		p.logger.Debug("retrieveParent: resolved pom from relative path tryRelativePath")
		return pom, nil
	}

	// If not found, search local/remote remoteRepositories
	p.logger.Debug("retrieveParent: tryRepository", log.String("target.GroupID", target.GroupID), log.String("target.artifactID", target.ArtifactID), log.String("target.Version", target.Version.String()))
	pom, err = p.tryRepository(target.GroupID, target.ArtifactID, target.Version.String())
	if err != nil {
		p.logger.Debug("retrieveParent: tryRepository error", log.Err(err))
		errs = multierror.Append(errs, err)
	} else {
		p.logger.Debug("retrieveParent: resolved pom from tryRepository")
		return pom, nil
	}

	// Reaching here means the POM wasn't found
	p.logger.Debug("retrieveParent: POM not found")
	return nil, errs
}

func (p *Parser) tryRelativePath(parentArtifact artifact, currentPath, relativePath string, visitedLocalPaths map[string]struct{}) (*pom, error) {
	p.logger.Debug("tryRelativePath", log.String("parentArtifact", parentArtifact.String()), log.String("currentPath", currentPath), log.String("relativePath", relativePath))

	pom, err := p.openRelativePom(currentPath, relativePath)

	if err != nil {
		return nil, err
	}

	p.logger.Debug("tryRelativePath: opened relative POM successfully")

	// To avoid an infinite loop or parsing the wrong parent when using relatedPath or `../pom.xml`,
	// we need to compare GAV of `parentArtifact` (`parent` tag from base pom) and GAV of pom from `relativePath`.
	// See `compare ArtifactIDs for base and parent pom's` test for example.
	// But GroupID can be inherited from parent (`p.analyze` function is required to get the GroupID).
	// Version can contain a property (`p.analyze` function is required to get the GroupID).
	// So we can only match ArtifactID's.
	if pom.artifact().ArtifactID != parentArtifact.ArtifactID {
		return nil, xerrors.New("'parent.relativePath' points at wrong local POM")
	}

	result, err := p.analyze(pom, analysisOptions{}, visitedLocalPaths)
	if err != nil {
		return nil, xerrors.Errorf("analyze error: %w", err)
	}

	if !parentArtifact.Equal(result.artifact) {
		return nil, xerrors.New("'parent.relativePath' points at wrong local POM")
	}

	return pom, nil
}

func (p *Parser) openRelativePom(currentPath, relativePath string) (*pom, error) {
	// e.g. child/pom.xml => child/
	dir := filepath.Dir(currentPath)

	// e.g. child + ../parent => parent/
	filePath := filepath.Join(dir, relativePath)

	isDir, err := isDirectory(filePath)
	if err != nil {
		return nil, err
	} else if isDir {
		// e.g. parent/ => parent/pom.xml
		filePath = filepath.Join(filePath, "pom.xml")
	}

	pom, err := p.openPom(filePath)
	if err != nil {
		return nil, xerrors.Errorf("failed to open %s: %w", filePath, err)
	}
	return pom, nil
}

func (p *Parser) openPom(filePath string) (*pom, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, xerrors.Errorf("file open error (%s): %w", filePath, err)
	}
	defer f.Close()

	content, err := parsePom(f)
	if err != nil {
		return nil, xerrors.Errorf("failed to parse the local POM: %w", err)
	}
	return &pom{
		filePath: filePath,
		content:  content,
	}, nil
}
func (p *Parser) tryRepository(groupID, artifactID, version string) (*pom, error) {
	if version == "" {
		return nil, xerrors.Errorf("Version missing for %s:%s", groupID, artifactID)
	}

	// Generate a proper path to the pom.xml
	// e.g. com.fasterxml.jackson.core, jackson-annotations, 2.10.0
	//      => com/fasterxml/jackson/core/jackson-annotations/2.10.0/jackson-annotations-2.10.0.pom
	paths := strings.Split(groupID, ".")
	paths = append(paths, artifactID, version, fmt.Sprintf("%s-%s.pom", artifactID, version))

	// Search local remoteRepositories
	loaded, err := p.loadPOMFromLocalRepository(paths)
	if err == nil {
		return loaded, nil
	}

	// Search remote remoteRepositories
	loaded, err = p.fetchPOMFromRemoteRepositories(paths, isSnapshot(version))
	if err == nil {
		return loaded, nil
	}

	return nil, xerrors.Errorf("%s:%s:%s was not found in local/remote repositories", groupID, artifactID, version)
}

func (p *Parser) loadPOMFromLocalRepository(paths []string) (*pom, error) {
	paths = append([]string{p.localRepository}, paths...)
	localPath := filepath.Join(paths...)

	return p.openPom(localPath)
}

func (p *Parser) fetchPOMFromRemoteRepositories(paths []string, snapshot bool) (*pom, error) {
	// Do not try fetching pom.xml from remote repositories in offline mode
	if p.offline {
		p.logger.Debug("Fetching the remote pom.xml is skipped")
		return nil, xerrors.New("offline mode")
	}

	remoteRepos := p.releaseRemoteRepos
	// Maven uses only snapshot repos for snapshot artifacts
	if snapshot {
		remoteRepos = p.snapshotRemoteRepos
	}

	// try all remoteRepositories
	for _, repo := range remoteRepos {
		repoPaths := slices.Clone(paths) // Clone slice to avoid overwriting last element of `paths`
		if snapshot {
			pomFileName, err := p.fetchPomFileNameFromMavenMetadata(repo, repoPaths)
			if err != nil {
				return nil, xerrors.Errorf("fetch maven-metadata.xml error: %w", err)
			}
			// Use file name from `maven-metadata.xml` if it exists
			if pomFileName != "" {
				repoPaths[len(repoPaths)-1] = pomFileName
			}
		}
		fetched, err := p.fetchPOMFromRemoteRepository(repo, repoPaths)
		if err != nil {
			return nil, xerrors.Errorf("fetch repository error: %w", err)
		} else if fetched == nil {
			continue
		}
		return fetched, nil
	}
	return nil, xerrors.Errorf("the POM was not found in remote remoteRepositories")
}

func (p *Parser) remoteRepoRequest(repo string, paths []string) (*http.Request, error) {
	repoURL, err := url.Parse(repo)
	if err != nil {
		return nil, xerrors.Errorf("unable to parse URL: %w", err)
	}

	paths = append([]string{repoURL.Path}, paths...)
	repoURL.Path = path.Join(paths...)

	req, err := http.NewRequest("GET", repoURL.String(), http.NoBody)
	if err != nil {
		return nil, xerrors.Errorf("unable to create HTTP request: %w", err)
	}
	if repoURL.User != nil {
		password, _ := repoURL.User.Password()
		req.SetBasicAuth(repoURL.User.Username(), password)
	}

	return req, nil
}

var client = &http.Client{}

func httpRequest(req *http.Request) ([]byte, int, error) {
	var resp *http.Response
	var err error
	var statusCode int = 0
	var data = []byte{}

	resp, err = client.Do(req)

	// HTTP request was made successfully (doesn't mean it was a 2xx, just that the client did not return an error)
	if err == nil {
		defer resp.Body.Close()

		statusCode = resp.StatusCode

		// Read response body
		data, err = io.ReadAll(resp.Body)

		if err != nil {
			return nil, statusCode, err
		}

		return data, statusCode, nil
	} else {
		// Error when making HTTP request
		return nil, statusCode, err
	}
}

// performs an HTTP request with caching support (if enabled)
func (p *Parser) cachedHTTPRequest(req *http.Request, path string) ([]byte, int, error) {
	var err error
	var statusCode int = 0
	var data = []byte{}

	// E.g. if the cache is disabled, make a regular HTTP request without caching
	if p.mavenHttpCache == nil {
		data, statusCode, err = httpRequest(req)
		return data, statusCode, err
	}

	url := req.URL.String()

	if entry, err := p.mavenHttpCache.get(path); err != nil {
		p.logger.Debug("Cache read error", log.String("url", url), log.String("path", path), log.Err(err))
	} else if entry != nil {
		p.logger.Debug("Cache hit", log.String("url", url), log.String("path", path))
		return entry.Data, entry.StatusCode, nil
	} else {
		p.logger.Debug("Cache miss, making HTTP request", log.String("url", url), log.String("path", path))
	}

	if p.mavenHttpCache.isDomainBlocklisted(req.URL.Host) {
		p.logger.Debug(
			fmt.Sprintf("Domain %s is blocklisted, assuming 404", req.URL.Host),
		)
		return nil, http.StatusNotFound, nil
	} else {
		data, statusCode, err = httpRequest(req)

		// Error when making HTTP request
		if err != nil {
			p.logger.Debug("HTTP error", log.String("url", url), log.String("path", path), log.Err(err))

			if strings.Contains(err.Error(), "i/o timeout") {
				p.mavenHttpCache.domainTimeouts[req.URL.Host]++

				p.logger.Debug(
					"I/O timeout, falling back to 404",
					log.Int(fmt.Sprintf("numTimeouts[%s]", req.URL.Host), p.mavenHttpCache.domainTimeouts[req.URL.Host]),
				)

				if p.mavenHttpCache.domainTimeouts[req.URL.Host] >= MaxDomainTimeouts {
					p.logger.Warn(
						fmt.Sprintf("Blocklisting domain %s due to too many timeouts", req.URL.Host),
					)

					err = p.mavenHttpCache.blocklistDomain(req.URL.Host)
				}

				return nil, http.StatusNotFound, err
			} else {
				return nil, statusCode, err
			}
		}
	}

	// Cache 2xx or 404 (we don't want to keep fetching artifacts that are not found via 404)
	if statusCode == http.StatusOK || statusCode == http.StatusNotFound {
		if cacheErr := p.mavenHttpCache.set(url, path, data, statusCode); cacheErr != nil {
			p.logger.Debug("Failed to cache response", log.String("url", url), log.String("path", path), log.Err(cacheErr))
		} else {
			p.logger.Debug("Cached response", log.String("url", url), log.String("path", path))
		}
	} else {
		p.logger.Debug("Response not successful, no caching", log.String("url", url), log.String("path", path), log.Int("statusCode", statusCode))
	}

	return data, statusCode, nil
}

// fetchPomFileNameFromMavenMetadata fetches `maven-metadata.xml` file to detect file name of pom file.
func (p *Parser) fetchPomFileNameFromMavenMetadata(repo string, paths []string) (string, error) {
	// Overwrite pom file name to `maven-metadata.xml`
	mavenMetadataPaths := slices.Clone(paths[:len(paths)-1]) // Clone slice to avoid shadow overwriting last element of `paths`
	mavenMetadataPaths = append(mavenMetadataPaths, "maven-metadata.xml")

	req, err := p.remoteRepoRequest(repo, mavenMetadataPaths)
	if err != nil {
		p.logger.Debug("Unable to create request", log.String("repo", repo), log.Err(err))
		return "", nil
	}

	data, statusCode, err := p.cachedHTTPRequest(req, strings.Join(mavenMetadataPaths, "/"))
	if err != nil {
		p.logger.Debug("Failed to fetch", log.String("url", req.URL.String()), log.Err(err))
		return "", nil
	} else if statusCode != http.StatusOK {
		p.logger.Debug("Failed to fetch", log.String("url", req.URL.String()), log.Int("statusCode", statusCode))
		return "", nil
	}

	mavenMetadata, err := parseMavenMetadata(strings.NewReader(string(data)))
	if err != nil {
		return "", xerrors.Errorf("failed to parse maven-metadata.xml file: %w", err)
	}

	var pomFileName string
	for _, sv := range mavenMetadata.Versioning.SnapshotVersions {
		if sv.Extension == "pom" {
			// mavenMetadataPaths[len(mavenMetadataPaths)-3] is always artifactID
			pomFileName = fmt.Sprintf("%s-%s.pom", mavenMetadataPaths[len(mavenMetadataPaths)-3], sv.Value)
		}
	}

	return pomFileName, nil
}

func (p *Parser) fetchPOMFromRemoteRepository(repo string, paths []string) (*pom, error) {
	req, err := p.remoteRepoRequest(repo, paths)
	if err != nil {
		p.logger.Debug("Unable to create request", log.String("repo", repo), log.Err(err))
		return nil, nil
	}

	data, statusCode, err := p.cachedHTTPRequest(req, strings.Join(paths, "/"))
	if err != nil {
		p.logger.Debug("Failed to fetch", log.String("url", req.URL.String()), log.Err(err))
		return nil, nil
	} else if statusCode != http.StatusOK {
		p.logger.Debug("Failed to fetch", log.String("url", req.URL.String()), log.Int("statusCode", statusCode))
		return nil, nil
	}

	content, err := parsePom(strings.NewReader(string(data)))
	if err != nil {
		return nil, xerrors.Errorf("failed to parse the remote POM: %w", err)
	}

	return &pom{
		filePath: "", // from remote repositories
		content:  content,
	}, nil
}

func parsePom(r io.Reader) (*pomXML, error) {
	parsed := &pomXML{}
	decoder := xml.NewDecoder(r)
	decoder.CharsetReader = charset.NewReaderLabel
	if err := decoder.Decode(parsed); err != nil {
		return nil, xerrors.Errorf("xml decode error: %w", err)
	}
	return parsed, nil
}

func parseMavenMetadata(r io.Reader) (*Metadata, error) {
	parsed := &Metadata{}
	decoder := xml.NewDecoder(r)
	decoder.CharsetReader = charset.NewReaderLabel
	if err := decoder.Decode(parsed); err != nil {
		return nil, xerrors.Errorf("xml decode error: %w", err)
	}
	return parsed, nil
}

func packageID(name, version string) string {
	return dependency.ID(ftypes.Pom, name, version)
}

// cf. https://github.com/apache/maven/blob/259404701402230299fe05ee889ecdf1c9dae816/maven-artifact/src/main/java/org/apache/maven/artifact/DefaultArtifact.java#L482-L486
func isSnapshot(ver string) bool {
	return strings.HasSuffix(ver, "SNAPSHOT") || ver == "LATEST"
}

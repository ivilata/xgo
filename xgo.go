// Go CGO cross compiler
// Copyright (c) 2014 Péter Szilágyi. All rights reserved.
//
// Released under the MIT license.

// Wrapper around the GCO cross compiler docker container.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/build"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Path where to cache external dependencies
var depsCache = filepath.Join(os.TempDir(), "xgo-cache")

// Cross compilation docker containers
var dockerBase = "karalabe/xgo-base"
var dockerDist = "karalabe/xgo-"

// Command line arguments to fine tune the compilation
var (
	goVersion   = flag.String("go", "latest", "Go release to use for cross compilation")
	inPackage   = flag.String("pkg", "", "Sub-package to build if not root import")
	outPrefix   = flag.String("out", "", "Prefix to use for output naming (empty = package name)")
	outFolder   = flag.String("dest", "", "Destination folder to put binaries in (empty = current)")
	srcRemote   = flag.String("remote", "", "Version control remote repository to build")
	srcBranch   = flag.String("branch", "", "Version control branch to build")
	crossDeps   = flag.String("deps", "", "CGO dependencies (configure/make based archives)")
	targets     = flag.String("targets", "*/*", "Comma separated targets to build for")
	dockerImage = flag.String("image", "", "Use custom docker image instead of official distribution")
)

// Command line arguments to pass to go build
var buildVerbose = flag.Bool("v", false, "Print the names of packages as they are compiled")
var buildSteps = flag.Bool("x", false, "Print the command as executing the builds")
var buildRace = flag.Bool("race", false, "Enable data race detection (supported only on amd64)")

func main() {
	flag.Parse()

	// Ensure docker is available
	if err := checkDocker(); err != nil {
		log.Fatalf("Failed to check docker installation: %v.", err)
	}
	// Validate the command line arguments
	if len(flag.Args()) != 1 {
		log.Fatalf("Usage: %s [options] <go import path>", os.Args[0])
	}
	// Select the image to use, either official or custom
	image := dockerDist + *goVersion
	if *dockerImage != "" {
		image = *dockerImage
	}
	// Check that all required images are available
	found, err := checkDockerImage(image)
	switch {
	case err != nil:
		log.Fatalf("Failed to check docker image availability: %v.", err)
	case !found:
		fmt.Println("not found!")
		if err := pullDockerImage(image); err != nil {
			log.Fatalf("Failed to pull docker image from the registry: %v.", err)
		}
	default:
		fmt.Println("found.")
	}
	// Cache all external dependencies to prevent always hitting the internet
	if *crossDeps != "" {
		if err := os.MkdirAll(depsCache, 751); err != nil {
			log.Fatalf("Failed to create dependency cache: %v.", err)
		}
		// Download all missing dependencies
		for _, dep := range strings.Split(*crossDeps, " ") {
			if url := strings.TrimSpace(dep); len(url) > 0 {
				path := filepath.Join(depsCache, filepath.Base(url))

				if _, err := os.Stat(path); err != nil {
					fmt.Printf("Downloading new dependency: %s...\n", url)

					out, err := os.Create(path)
					if err != nil {
						log.Fatalf("Failed to create dependency file: %v.", err)
					}
					res, err := http.Get(url)
					if err != nil {
						log.Fatalf("Failed to retrieve dependency: %v.", err)
					}
					defer res.Body.Close()

					if _, err := io.Copy(out, res.Body); err != nil {
						log.Fatalf("Failed to download dependency: %v", err)
					}
					out.Close()

					fmt.Printf("New dependency cached: %s.\n", path)
				} else {
					fmt.Printf("Dependency already cached: %s.\n", path)
				}
			}
		}
	}
	// Cross compile the requested package into the local folder
	if err := compile(flag.Args()[0], image, *srcRemote, *srcBranch, *inPackage, *crossDeps, *outFolder, *outPrefix, *buildVerbose, *buildSteps, *buildRace, strings.Split(*targets, ",")); err != nil {
		log.Fatalf("Failed to cross compile package: %v.", err)
	}
}

// Checks whether a docker installation can be found and is functional.
func checkDocker() error {
	fmt.Println("Checking docker installation...")
	if err := run(exec.Command("docker", "version")); err != nil {
		return err
	}
	fmt.Println()
	return nil
}

// Checks whether a required docker image is available locally.
func checkDockerImage(image string) (bool, error) {
	fmt.Printf("Checking for required docker image %s... ", image)
	out, err := exec.Command("docker", "images", "--no-trunc").Output()
	if err != nil {
		return false, err
	}
	return bytes.Contains(out, []byte(image)), nil
}

// Pulls an image from the docker registry.
func pullDockerImage(image string) error {
	fmt.Printf("Pulling %s from docker registry...\n", image)
	return run(exec.Command("docker", "pull", image))
}

// Cross compiles a requested package into the current working directory.
func compile(repo string, image string, remote string, branch string, pack string, deps string, dest string, prefix string, verbose bool, steps bool, race bool, targets []string) error {
	// Retrieve the current folder to store the binaries in
	folder, err := os.Getwd()
	if err != nil {
		log.Fatalf("Failed to retrieve the working directory: %v.", err)
	}
	if dest != "" {
		folder, err = filepath.Abs(dest)
		if err != nil {
			log.Fatalf("Failed to resolve destination path (%s): %v.", dest, err)
		}
	}
	// If a local build was requested, find the import path and mount all GOPATH sources
	locals, mounts, paths := []string{}, []string{}, []string{}
	if strings.HasPrefix(repo, string(filepath.Separator)) || strings.HasPrefix(repo, ".") {
		// Resolve the repository import path from the file path
		path, err := filepath.Abs(repo)
		if err != nil {
			log.Fatalf("Failed to locate requested package: %v.", err)
		}
		stat, err := os.Stat(path)
		if err != nil || !stat.IsDir() {
			log.Fatalf("Requested path invalid.")
		}
		pack, err := build.ImportDir(path, build.FindOnly)
		if err != nil {
			log.Fatalf("Failed to resolve import path: %v.", err)
		}
		repo = pack.ImportPath

		// Iterate over all the local libs and export the mount points
		for _, gopath := range strings.Split(os.Getenv("GOPATH"), string(os.PathListSeparator)) {
			// Since docker sandboxes volumes, resolve any symlinks manually
			sources := filepath.Join(gopath, "src")
			filepath.Walk(sources, func(path string, info os.FileInfo, err error) error {
				// Skip anything that's not a symlink
				if info.Mode()&os.ModeSymlink == 0 {
					return nil
				}
				// Resolve the symlink and skip if it's not a folder
				target, err := filepath.EvalSymlinks(path)
				if err != nil {
					return nil
				}
				if info, err = os.Stat(target); err != nil || !info.IsDir() {
					return nil
				}
				// Skip if the symlink points within GOPATH
				if filepath.HasPrefix(target, sources) {
					return nil
				}
				// Folder needs explicit mounting due to docker symlink security
				locals = append(locals, target)
				mounts = append(mounts, filepath.Join("/ext-go", strconv.Itoa(len(locals)), "src", strings.TrimPrefix(path, sources)))
				paths = append(paths, filepath.Join("/ext-go", strconv.Itoa(len(locals))))
				return nil
			})
			// Export the main mount point for this GOPATH entry
			locals = append(locals, sources)
			mounts = append(mounts, filepath.Join("/ext-go", strconv.Itoa(len(locals)), "src"))
			paths = append(paths, filepath.Join("/ext-go", strconv.Itoa(len(locals))))
		}
	}
	// Assemble and run the cross compilation command
	fmt.Printf("Cross compiling %s...\n", repo)

	args := []string{
		"run", "--rm",
		"-v", folder + ":/build",
		"-v", depsCache + ":/deps-cache:ro",
		"-e", "REPO_REMOTE=" + remote,
		"-e", "REPO_BRANCH=" + branch,
		"-e", "PACK=" + pack,
		"-e", "DEPS=" + deps,
		"-e", "OUT=" + prefix,
		"-e", fmt.Sprintf("FLAG_V=%v", verbose),
		"-e", fmt.Sprintf("FLAG_X=%v", steps),
		"-e", fmt.Sprintf("FLAG_RACE=%v", race),
		"-e", "TARGETS=" + strings.Replace(strings.Join(targets, " "), "*", ".", -1),
	}
	for i := 0; i < len(locals); i++ {
		args = append(args, []string{"-v", fmt.Sprintf("%s:%s:ro", locals[i], mounts[i])}...)
	}
	args = append(args, []string{"-e", "EXT_GOPATH=" + strings.Join(paths, ":")}...)

	args = append(args, []string{image, repo}...)
	return run(exec.Command("docker", args...))
}

// Executes a command synchronously, redirecting its output to stdout.
func run(cmd *exec.Cmd) error {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

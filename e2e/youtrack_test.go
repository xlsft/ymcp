//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The throwaway YouTrack signs in with the admin account a clean install
// creates when the configuration wizard is skipped.
const throwawayLogin, throwawayPassword = "admin", "admin"

// startYouTrack runs the image in youtrack/Dockerfile in a container of the
// run's own, with the configuration wizard skipped, and waits until it
// answers. stop removes the container and its configuration volume.
func startYouTrack() (url string, stop func(), err error) {
	image, err := youtrackImage()
	if err != nil {
		return "", nil, err
	}
	port, err := listenPort()
	if err != nil {
		return "", nil, err
	}
	url = "http://127.0.0.1:" + port
	name := marker + "-youtrack-" + runID
	conf := name + "-conf"
	stop = func() {
		exec.Command("docker", "rm", "-f", name).Run()
		exec.Command("docker", "volume", "rm", "-f", conf).Run()
	}
	docker := func(args ...string) error {
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("docker %s: %v\n%s", args[0], err, out)
		}
		return nil
	}

	fmt.Printf("e2e: starting %s at %s\n", image, url)
	start := time.Now()
	if err := docker("pull", "-q", image); err != nil {
		return "", nil, err
	}
	// configure writes to the volume what the container started next reads.
	if err := docker("run", "--rm", "-v", conf+":/opt/youtrack/conf", image, "configure",
		"-J-Ddisable.configuration.wizard.on.clean.install=true", "--base-url="+url); err != nil {
		stop()
		return "", nil, err
	}
	if err := docker("run", "-d", "--name", name, "-v", conf+":/opt/youtrack/conf",
		"-p", "127.0.0.1:"+port+":8080", image); err != nil {
		stop()
		return "", nil, err
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		resp, err := http.Get(url + "/api/config?fields=version")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			out, _ := exec.Command("docker", "logs", "--tail", "50", name).CombinedOutput()
			stop()
			return "", nil, fmt.Errorf("YouTrack didn't come up in 5 minutes:\n%s", out)
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Printf("e2e: YouTrack is up after %s\n", time.Since(start).Round(time.Second))
	return url, stop, nil
}

// youtrackImage is the image the FROM line of youtrack/Dockerfile names.
func youtrackImage() (string, error) {
	f, err := os.Open("youtrack/Dockerfile")
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if image, ok := strings.CutPrefix(strings.TrimSpace(s.Text()), "FROM "); ok {
			return strings.TrimSpace(image), nil
		}
	}
	return "", fmt.Errorf("youtrack/Dockerfile has no FROM line")
}

// exitUnavailable is the exit code of a run that the instance's outage
// cut short. The workflow reports it apart from failures (see e2e.yml).
const exitUnavailable = 3

// outageText finds the answers of an instance that is down, restarting or
// in read-only mode for maintenance, as ymcp's errors put them.
var outageText = regexp.MustCompile(`(?i): (418|502|503|504)\b|read-only mode|maintenance|connection refused|connection reset`)

var (
	outageMu sync.Mutex
	// outage is the first error of the run that says the instance is
	// unavailable; empty while it answers.
	outage string
)

// noteOutage remembers an error that says the instance is unavailable.
func noteOutage(text string) {
	if !outageText.MatchString(text) {
		return
	}
	outageMu.Lock()
	defer outageMu.Unlock()
	if outage == "" {
		outage = text
	}
}

func outageSeen() string {
	outageMu.Lock()
	defer outageMu.Unlock()
	return outage
}

// waitAvailable waits until the instance takes a write, for up to wait: a
// read-only instance still answers reads. It creates and deletes a marked
// tag, and reports the last outage error if the instance never took it.
func waitAvailable(wait time.Duration) error {
	t := &sweepT{}
	name := marker + " probe " + runID
	deadline := time.Now().Add(wait)
	for {
		r := call(t, "create_tag", map[string]any{"name": name})
		if !r.isError {
			var tag struct{ ID string }
			json.Unmarshal([]byte(r.text), &tag)
			call(t, "delete_tag", map[string]any{"tag": tag.ID})
			outageMu.Lock()
			outage = "" // what the wait saw is over
			outageMu.Unlock()
			return nil
		}
		if !outageText.MatchString(r.text) {
			return nil // not an outage; the tests will say what is wrong
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s", shortText(r.text))
		}
		fmt.Printf("e2e: YouTrack is unavailable, waiting: %s\n", shortText(r.text))
		time.Sleep(15 * time.Second)
	}
}

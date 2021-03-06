package main

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	homedir "github.com/mitchellh/go-homedir"
	cli "gopkg.in/urfave/cli.v2"
)

const baseLoginURL = "https://dash.cloudflare.com/warp"
const baseCertStoreURL = "https://login.cloudflarewarp.com"
const clientTimeout = time.Minute * 20

func login(c *cli.Context) error {
	configPath, err := homedir.Expand(defaultConfigDirs[0])
	if err != nil {
		return err
	}
	ok, err := fileExists(configPath)
	if !ok && err == nil {
		// create config directory if doesn't already exist
		err = os.Mkdir(configPath, 0700)
	}
	if err != nil {
		return err
	}
	path := filepath.Join(configPath, defaultCredentialFile)
	fileInfo, err := os.Stat(path)
	if err == nil && fileInfo.Size() > 0 {
		fmt.Fprintf(os.Stderr, `You have an existing certificate at %s which login would overwrite.
If this is intentional, please move or delete that file then run this command again.
`, path)
		return nil
	}
	if err != nil && err.(*os.PathError).Err != syscall.ENOENT {
		return err
	}

	// for local debugging
	baseURL := baseCertStoreURL
	if c.IsSet("url") {
		baseURL = c.String("url")
	}
	// Generate a random post URL
	certURL := baseURL + generateRandomPath()
	loginURL, err := url.Parse(baseLoginURL)
	if err != nil {
		// shouldn't happen, URL is hardcoded
		return err
	}
	loginURL.RawQuery = "callback=" + url.QueryEscape(certURL)

	err = open(loginURL.String())
	if err != nil {
		fmt.Fprintf(os.Stderr, `Please open the following URL and log in with your Cloudflare account:

%s

Leave cloudflared running to install the certificate automatically.
`, loginURL.String())
	} else {
		fmt.Fprintf(os.Stderr, `A browser window should have opened at the following URL:

%s

If the browser failed to open, open it yourself and visit the URL above.

`, loginURL.String())
	}

	if download(certURL, path) {
		fmt.Fprintf(os.Stderr, `You have successfully logged in.
If you wish to copy your credentials to a server, they have been saved to:
%s
`, path)
	} else {
		fmt.Fprintf(os.Stderr, `Failed to write the certificate due to the following error:
%v

Your browser will download the certificate instead. You will have to manually
copy it to the following path:

%s

`, err, path)
	}
	return nil
}

// generateRandomPath generates a random URL to associate with the certificate.
func generateRandomPath() string {
	randomBytes := make([]byte, 40)
	_, err := rand.Read(randomBytes)
	if err != nil {
		panic(err)
	}
	return "/" + base32.StdEncoding.EncodeToString(randomBytes)
}

// open opens the specified URL in the default browser of the user.
func open(url string) error {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start"}
	case "darwin":
		cmd = "open"
	default: // "linux", "freebsd", "openbsd", "netbsd"
		cmd = "xdg-open"
	}
	args = append(args, url)
	return exec.Command(cmd, args...).Start()
}

func download(certURL, filePath string) bool {
	client := &http.Client{Timeout: clientTimeout}
	// attempt a (long-running) certificate get
	for i := 0; i < 20; i++ {
		ok, err := tryDownload(client, certURL, filePath)
		if ok {
			putSuccess(client, certURL)
			return true
		}
		if err != nil {
			logger.WithError(err).Error("Error fetching certificate")
			return false
		}
	}
	return false
}

func tryDownload(client *http.Client, certURL, filePath string) (ok bool, err error) {
	resp, err := client.Get(certURL)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return false, nil
	}
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("Unexpected HTTP error code %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/x-pem-file" {
		return false, fmt.Errorf("Unexpected content type %s", resp.Header.Get("Content-Type"))
	}
	// write response
	file, err := os.Create(filePath)
	if err != nil {
		return false, err
	}
	defer file.Close()
	written, err := io.Copy(file, resp.Body)
	switch {
	case err != nil:
		return false, err
	case resp.ContentLength != written && resp.ContentLength != -1:
		return false, fmt.Errorf("Short read (%d bytes) from server while writing certificate", written)
	default:
		return true, nil
	}
}

func putSuccess(client *http.Client, certURL string) {
	// indicate success to the relay server
	req, err := http.NewRequest("PUT", certURL+"/ok", nil)
	if err != nil {
		logger.WithError(err).Error("HTTP request error")
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		logger.WithError(err).Error("HTTP error")
		return
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		logger.Errorf("Unexpected HTTP error code %d", resp.StatusCode)
	}
}

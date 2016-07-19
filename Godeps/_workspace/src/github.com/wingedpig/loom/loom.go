/*
Package loom implements a set of functions to interact with remote servers using SSH.
It is based on the Python fabric library.
*/
package loom

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/nofdev/fastforward/Godeps/_workspace/src/golang.org/x/crypto/ssh"
)

// Config contains ssh and other configuration data needed for all the public functions in loom.
type Config struct {
	// The user name used in SSH connections. If not specified, the current user is assumed.
	User string

	// Password for SSH connections. This is optional. If the user has an ~/.ssh/id_rsa keyfile,
	// that will also be tried. In addition, other key files can be specified.
	Password string

	// The machine:port to connect to.
	Host string

	// The file names of additional key files to use for authentication (~/.ssh/id_rsa is defaulted).
	// RSA (PKCS#1), DSA (OpenSSL), and ECDSA private keys are supported.
	KeyFiles []string

	// If true, send command output to stdout.
	DisplayOutput bool

	// If true, errors are fatal and will abort immediately.
	AbortOnError bool
}

// parsekey is a private function that reads in a keyfile containing a private key and parses it.
func parsekey(file string) (ssh.Signer, error) {
	var private ssh.Signer
	privateBytes, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}

	private, err = ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		return nil, err
	}
	return private, nil
}

// connect is a private function to set up the ssh connection. It is called at the beginning of every public
// function.
func (config *Config) connect() (*ssh.Session, error) {

	sshconfig := &ssh.ClientConfig{
		User: config.User,
	}

	if config.User == "" {
		u, err := user.Current()
		if err != nil {
			return nil, err
		}
		sshconfig.User = u.Username
	}

	if config.Password != "" {
		sshconfig.Auth = append(sshconfig.Auth, ssh.Password(config.Password))
	}

	var pkey ssh.Signer
	var keyfile string
	var err error

	// Include additional key files
	for _, keyfile = range config.KeyFiles {
		pkey, err = parsekey(keyfile)
		if err != nil {
			if config.AbortOnError == true {
				log.Fatalf("%s", err)
			}
			return nil, err
		}
		sshconfig.Auth = append(sshconfig.Auth, ssh.PublicKeys(pkey))
	}

	// By default, we try to include ~/.ssh/id_rsa. It is not an error if this file
	// doesn't exist.
	keyfile = os.Getenv("HOME") + "/.ssh/id_rsa"
	pkey, err = parsekey(keyfile)
	if err == nil {
		fmt.Println("Adding default rsa key")
		sshconfig.Auth = append(sshconfig.Auth, ssh.PublicKeys(pkey))
	}

	host := config.Host
	if strings.Contains(host, ":") == false {
		host = host + ":22"
	}
	client, err := ssh.Dial("tcp", host, sshconfig)
	if err != nil {
		if config.AbortOnError == true {
			log.Fatalf("%s", err)
		}
		return nil, err
	}

	session, err := client.NewSession()
	if err != nil {
		if config.AbortOnError == true {
			log.Fatalf("%s", err)
		}
		return nil, err
	}
	return session, err
}

// doRun is called by both Run() and Sudo() to execute a command.
func (config *Config) doRun(cmd string, sudo bool) (string, error) {

	session, err := config.connect()
	if err != nil {
		if config.AbortOnError == true {
			log.Fatalf("%s", err)
		}
		return "", err
	}
	defer session.Close()

	// Set up terminal modes
	modes := ssh.TerminalModes{
		ssh.ECHO:          0,     // disable echoing
		ssh.TTY_OP_ISPEED: 14400, // input speed = 14.4kbaud
		ssh.TTY_OP_OSPEED: 14400, // output speed = 14.4kbaud
	}
	// Request pseudo terminal
	if err := session.RequestPty("xterm", 80, 40, modes); err != nil {
		if config.AbortOnError == true {
			log.Fatalf("%s", err)
		}
		return "", err
	}

	if sudo == true {
		cmd = fmt.Sprintf("/usr/bin/sudo bash <<CMD\nexport PATH=/usr/local/sbin:/usr/local/bin:/sbin:/bin:/usr/sbin:/usr/bin:/root/bin\n%s\nCMD", cmd)
	}

	// TODO: use pipes instead of CombinedOutput so that we can show the output of commands more interactively, instead
	// of now, which is after they're completely done executing.
	output, err := config.executeCommand(session, cmd, sudo)
	soutput := strings.Replace(string(output), "\r\n", "\n", -1)
	if err != nil {
		if config.DisplayOutput == true && len(soutput) > 0 {
			fmt.Printf("%s", soutput)
		}
		if config.AbortOnError == true {
			log.Fatalf("%s", err)
		}
		return "", err
	}
	session.SendRequest("close", false, nil)
	if config.DisplayOutput == true {
		fmt.Printf("%s", soutput)
	}
	return soutput, nil
}

type singleWriterReader struct {
	b         *bytes.Buffer
	b2        *bytes.Buffer
	teeReader io.Reader
	mu        sync.Mutex
	usingRead bool
}

func newSingleWriterReader() singleWriterReader {
	b := bytes.NewBuffer([]byte{})
	b2 := bytes.NewBuffer([]byte{})
	return singleWriterReader{b, b2, io.TeeReader(b, b2), sync.Mutex{}, false}
}

func (w *singleWriterReader) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *singleWriterReader) Read(p []byte) (int, error) {
	w.usingRead = true
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.teeReader.Read(p)
	return n, err
}

// Note: This should only be read once at the end, after everything has
// be written or read.
func (w *singleWriterReader) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append(w.b2.Bytes(), w.b.Bytes()...)
}

func (config *Config) executeCommand(s *ssh.Session, cmd string, sudo bool) ([]byte, error) {
	if s.Stdout != nil {
		return nil, errors.New("ssh: Stdout already set")
	}
	if s.Stderr != nil {
		return nil, errors.New("ssh: Stderr already set")
	}

	b := newSingleWriterReader()
	s.Stdout = &b
	s.Stderr = &b
	done := make(chan bool)

	if sudo {
		stdInWriter, err := s.StdinPipe()
		if err != nil {
			if config.AbortOnError == true {
				log.Fatalf("%s", err)
			}
			return nil, err
		}

		go config.injectSudoPasswordIfNecessary(done, &b, stdInWriter)
	}

	err := s.Run(cmd)
	close(done)
	return b.Bytes(), err
}

func (config *Config) injectSudoPasswordIfNecessary(done <-chan bool, stdOutReader io.Reader, stdInWriter io.Writer) error {
	matchFound := false
	sudoMatcher := newSudoMatcher(config.User)
	for matchFound == false {
		select {
		case <-done:
		default:
			bytesRead := make([]byte, sudoMatcher.totalMatchLength)
			_, err := stdOutReader.Read(bytesRead)
			if err == io.EOF {
				continue
			}

			if err != nil {
				return err
			}
			matchFound = sudoMatcher.Match(bytesRead)
			if matchFound {
				stdInWriter.Write([]byte(config.Password + "\n"))
				break
			}
		}
	}

	return nil
}

type sudoMatcher struct {
	currentIndexMatch int
	currentPrompt     string
	stringToFind      string
	totalMatchLength  int
}

func newSudoMatcher(user string) sudoMatcher {
	stringToFind := fmt.Sprintf("[sudo] password for %s:", user)
	totalMatchLength := len([]byte(stringToFind))
	return sudoMatcher{0, "", stringToFind, totalMatchLength}
}

func (m *sudoMatcher) Match(additionalBytes []byte) bool {
	readString := string(additionalBytes)
	for _, runeVal := range readString {
		if runeVal == rune(m.stringToFind[m.currentIndexMatch]) {
			m.currentPrompt = m.currentPrompt + string(runeVal)
			m.currentIndexMatch++
		} else {
			m.currentPrompt = ""
			m.currentIndexMatch = 0
		}

		if len(m.currentPrompt) == m.totalMatchLength {
			return true
		}
	}
	return false
}

// Run takes a command and runs it on the remote host, using ssh.
func (config *Config) Run(cmd string) (string, error) {
	if config.DisplayOutput == true {
		fmt.Printf("run: %s\n", cmd)
	}
	return config.doRun(cmd, false)
}

// Sudo takes a command and runs it as root on the remote host, using sudo over ssh.
func (config *Config) Sudo(cmd string) (string, error) {
	if config.DisplayOutput == true {
		fmt.Printf("sudo: %s\n", cmd)
	}
	return config.doRun(cmd, true)
}

// Put copies one or more local files to the remote host, using scp. localfiles can
// contain wildcards, and remotefile can be either a directory or a file.
func (config *Config) Put(localfiles string, remotefile string) error {

	files, err := filepath.Glob(localfiles)
	if err != nil {
		if config.AbortOnError == true {
			log.Fatalf("%s", err)
		}
		return err
	}
	if len(files) == 0 {
		err = fmt.Errorf("No files match %s", localfiles)
		if config.AbortOnError == true {
			log.Fatalf("%s", err)
		}
		return err
	}
	for _, localfile := range files {

		fmt.Printf("put: %s %s\n", localfile, remotefile)
		contents, err := ioutil.ReadFile(localfile)
		if err != nil {
			if config.AbortOnError == true {
				log.Fatalf("%s", err)
			}
			return err
		}

		// get the local file mode bits
		fi, err := os.Stat(localfile)
		if err != nil {
			if config.AbortOnError == true {
				log.Fatalf("%s", err)
			}
			return err
		}
		// the file mode bits are the 9 least significant bits of Mode()
		mode := fi.Mode() & 1023

		session, err := config.connect()
		if err != nil {
			if config.AbortOnError == true {
				log.Fatalf("%s", err)
			}
			return err
		}
		var stdoutBuf bytes.Buffer
		var stderrBuf bytes.Buffer
		session.Stdout = &stdoutBuf
		session.Stderr = &stderrBuf

		w, _ := session.StdinPipe()

		_, lfile := filepath.Split(localfile)
		err = session.Start("/usr/bin/scp -qrt " + remotefile)
		if err != nil {
			w.Close()
			session.Close()
			if config.AbortOnError == true {
				log.Fatalf("%s", err)
			}
			return err
		}
		fmt.Fprintf(w, "C%04o %d %s\n", mode, len(contents), lfile /*remotefile*/)
		w.Write(contents)
		fmt.Fprint(w, "\x00")
		w.Close()

		err = session.Wait()
		if err != nil {
			if config.AbortOnError == true {
				log.Fatalf("%s", err)
			}
			session.Close()
			return err
		}

		if config.DisplayOutput == true {
			stdout := stdoutBuf.String()
			stderr := stderrBuf.String()
			fmt.Printf("%s%s", stderr, stdout)
		}
		session.Close()

	}

	return nil
}

// PutString generates a new file on the remote host containing data. The file is created with mode 0644.
func (config *Config) PutString(data string, remotefile string) error {

	if config.DisplayOutput == true {
		fmt.Printf("putstring: %s\n", remotefile)
	}
	session, err := config.connect()
	if err != nil {
		if config.AbortOnError == true {
			log.Fatalf("%s", err)
		}
		return err
	}
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf

	w, _ := session.StdinPipe()

	_, rfile := filepath.Split(remotefile)
	err = session.Start("/usr/bin/scp -qrt " + remotefile)
	if err != nil {
		w.Close()
		session.Close()
		if config.AbortOnError == true {
			log.Fatalf("%s", err)
		}
		return err
	}
	fmt.Fprintf(w, "C0644 %d %s\n", len(data), rfile)
	w.Write([]byte(data))
	fmt.Fprint(w, "\x00")
	w.Close()

	err = session.Wait()
	if err != nil {
		if config.AbortOnError == true {
			log.Fatalf("%s", err)
		}
		session.Close()
		return err
	}

	if config.DisplayOutput == true {
		stdout := stdoutBuf.String()
		stderr := stderrBuf.String()
		fmt.Printf("%s%s", stderr, stdout)
	}
	session.Close()

	return nil
}

// Get copies the file from the remote host to the local host, using scp. Wildcards are not currently supported.
func (config *Config) Get(remotefile string, localfile string) error {

	if config.DisplayOutput == true {
		fmt.Printf("get: %s %s\n", remotefile, localfile)
	}

	session, err := config.connect()
	if err != nil {
		if config.AbortOnError == true {
			log.Fatalf("%s", err)
		}
		return err
	}
	defer session.Close()

	// TODO: Handle wildcards in remotefile

	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf
	w, _ := session.StdinPipe()

	err = session.Start("/usr/bin/scp -qrf " + remotefile)
	if err != nil {
		w.Close()
		if config.AbortOnError == true {
			log.Fatalf("%s", err)
		}
		return err
	}
	// TODO: better error checking than just firing and forgetting these nulls.
	fmt.Fprintf(w, "\x00")
	fmt.Fprintf(w, "\x00")
	fmt.Fprintf(w, "\x00")
	fmt.Fprintf(w, "\x00")
	fmt.Fprintf(w, "\x00")
	fmt.Fprintf(w, "\x00")

	err = session.Wait()
	if err != nil {
		if config.AbortOnError == true {
			log.Fatalf("%s", err)
		}
		return err
	}

	stdout := stdoutBuf.String()
	//stderr := stderrBuf.String()

	// first line of stdout contains file information
	fields := strings.SplitN(stdout, "\n", 2)
	mode, _ := strconv.ParseInt(fields[0][1:5], 8, 32)

	// need to generate final local file name
	// localfile could be a directory or a filename
	// if it's a directory, we need to append the remotefile filename
	// if it doesn't exist, we assume file
	var lfile string
	_, rfile := filepath.Split(remotefile)
	l := len(localfile)
	if localfile[l-1] == '/' {
		localfile = localfile[:l-1]
	}
	fi, err := os.Stat(localfile)
	if err != nil || fi.IsDir() == false {
		lfile = localfile
	} else if fi.IsDir() == true {
		lfile = localfile + "/" + rfile
	}
	// there's a trailing 0 in the file that we need to nuke
	l = len(fields[1])
	err = ioutil.WriteFile(lfile, []byte(fields[1][:l-1]), os.FileMode(mode))
	if err != nil {
		if config.AbortOnError == true {
			log.Fatalf("%s", err)
		}
		return err
	}
	return nil
}

// Local executes a command on the local host.
func (config *Config) Local(cmd string) (string, error) {
	if config.DisplayOutput == true {
		fmt.Printf("local: %s\n", cmd)
	}
	var command *exec.Cmd
	command = exec.Command("/bin/sh", "-c", cmd)
	var out bytes.Buffer
	command.Stdout = &out
	err := command.Run()
	if err != nil {
		if config.AbortOnError == true {
			log.Fatalf("%s", err)
		}
		return "", err
	}
	if config.DisplayOutput == true {
		fmt.Printf("%s", out.String())
	}
	return out.String(), nil
}

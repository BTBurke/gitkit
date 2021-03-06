package gitkit

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Regular expression to match incoming git-over-ssh commands
var gitCommandRegex = regexp.MustCompile(`^(git[-|\s]upload-pack|git[-|\s]upload-archive|git[-|\s]receive-pack) '(.*)'$`)

type PublicKey struct {
	Id          string
	Name        string
	Fingerprint string
	Content     string
}

type SSH struct {
	sshconfig           *ssh.ServerConfig
	config              *config
	PublicKeyLookupFunc func(string) (*PublicKey, error)
}

type GitCommand struct {
	Namespace string
	Command   string
	Repo      string
}

func parseGitCommand(cmd string) (*GitCommand, error) {
	log.Printf("Received git command: %s\n", cmd)
	matches := gitCommandRegex.FindAllStringSubmatch(cmd, 1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("invalid git command")
	}
	repoWithNamespace := matches[0][2]
	splitRepo := strings.SplitAfter(repoWithNamespace, "/")
	namespace := strings.Join(splitRepo[0:len(splitRepo)-1], "")
	repo := splitRepo[len(splitRepo)-1]

	return &GitCommand{namespace, matches[0][1], repo}, nil
}

func NewSSH(config config) *SSH {

	s := &SSH{config: &config, PublicKeyLookupFunc: config.SSHPubKeyFunc}

	return s
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || os.IsExist(err)
}

func cleanCommand(cmd string) string {
	i := strings.Index(cmd, "git")
	if i == -1 {
		return cmd
	}
	return cmd[i:]
}

func execCommandBytes(cmdname string, args ...string) ([]byte, []byte, error) {
	bufOut := new(bytes.Buffer)
	bufErr := new(bytes.Buffer)

	cmd := exec.Command(cmdname, args...)
	cmd.Stdout = bufOut
	cmd.Stderr = bufErr

	err := cmd.Run()
	return bufOut.Bytes(), bufErr.Bytes(), err
}

func execCommand(cmdname string, args ...string) (string, string, error) {
	bufOut, bufErr, err := execCommandBytes(cmdname, args...)
	return string(bufOut), string(bufErr), err
}

func (s *SSH) handleConnection(keyID string, chans <-chan ssh.NewChannel) {
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			newChan.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}

		ch, reqs, err := newChan.Accept()
		if err != nil {
			log.Printf("error accepting channel: %v", err)
			continue
		}

		go func(in <-chan *ssh.Request) {
			defer ch.Close()

			for req := range in {

				payload := cleanCommand(string(req.Payload))

				switch req.Type {
				case "env":
					args := strings.Split(strings.Replace(payload, "\x00", "", -1), "\v")
					if len(args) != 2 {
						log.Printf("env: invalid env arguments: '%#v'", args)
						continue
					}

					args[0] = strings.TrimLeft(args[0], "\x04")

					_, _, err := execCommandBytes("env", args[0]+"="+args[1])
					if err != nil {
						log.Printf("env: %v", err)
						return
					}
				case "exec":
					log.Printf("Received raw command: %s", payload)
					cmdName := strings.TrimLeft(payload, "'()")
					log.Printf("ssh: payload '%v'", cmdName)

					if strings.HasPrefix(cmdName, "\x00") {
						cmdName = strings.Replace(cmdName, "\x00", "", -1)[1:]
					}

					gitcmd, err := parseGitCommand(cmdName)
					if err != nil {
						log.Println("ssh: error parsing command:", err)
						ch.Write([]byte("Invalid command.\r\n"))
						return
					}

					if s.config.SSHAuthFunc != nil {
						cmdAuthorized, err := s.config.SSHAuthFunc(keyID, gitcmd)
						if !cmdAuthorized || err != nil {
							log.Println("ssh: command not authorized")
							ch.Write([]byte("The command is not authorized for this repository.\r\n"))
							return
						}
					}
					log.Printf("Action on repo: %s", gitcmd.Repo)
					if !repoExists(filepath.Join(s.config.Dir, gitcmd.Repo)) && s.config.AutoCreate == true && gitcmd.Command == "git-receive-pack" {
						err := initRepo(gitcmd.Repo, s.config)
						if err != nil {
							logError("repo-init", err)
							return
						}
					}

					cmd := exec.Command(gitcmd.Command, gitcmd.Repo)
					log.Printf("SSH running in namespace: %s repo: %s\n ", gitcmd.Namespace, gitcmd.Repo)
					cmd.Dir = filepath.Join(s.config.Dir, gitcmd.Namespace)
					log.Printf("Changed dir to %s", cmd.Dir)
					cmd.Env = append(os.Environ(), "GITKIT_KEY="+keyID)
					// cmd.Env = append(os.Environ(), "SSH_ORIGINAL_COMMAND="+cmdName)

					stdout, err := cmd.StdoutPipe()
					if err != nil {
						log.Printf("ssh: cant open stdout pipe: %v", err)
						return
					}

					stderr, err := cmd.StderrPipe()
					if err != nil {
						log.Printf("ssh: cant open stderr pipe: %v", err)
						return
					}

					input, err := cmd.StdinPipe()
					if err != nil {
						log.Printf("ssh: cant open stdin pipe: %v", err)
						return
					}

					if err = cmd.Start(); err != nil {
						log.Printf("ssh: start error: %v", err)
						return
					}

					req.Reply(true, nil)
					go io.Copy(input, ch)
					io.Copy(ch, stdout)
					io.Copy(ch.Stderr(), stderr)

					if err = cmd.Wait(); err != nil {
						log.Printf("ssh: command failed: %v", err)
						return
					}

					ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
					return
				default:
					ch.Write([]byte("Unsupported request type.\r\n"))
					log.Println("ssh: unsupported req type:", req.Type)
					return
				}
			}
		}(reqs)
	}
}

func (s *SSH) createServerKey() error {
	if err := os.MkdirAll(s.config.KeyDir, os.ModePerm); err != nil {
		return err
	}
	out, err := exec.Command("ssh-keygen", "-f", s.config.KeyPath(), "-t", "rsa", "-N", "").CombinedOutput()
	if err != nil {
		return fmt.Errorf("key creating failed: %s", out)
	}
	return nil
}

func (s *SSH) setup() error {
	config := &ssh.ServerConfig{
		ServerVersion: fmt.Sprintf("SSH-2.0-gitkit %s", Version),
	}

	if s.config.KeyDir == "" {
		return fmt.Errorf("key directory is not provided")
	}

	if !s.config.SSHAuth {
		config.NoClientAuth = true
	} else {
		if s.PublicKeyLookupFunc == nil {
			return fmt.Errorf("public key lookup func is not provided")
		}

		config.PublicKeyCallback = func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			pkey, err := s.PublicKeyLookupFunc(strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))))
			if err != nil {
				return nil, err
			}

			if pkey == nil {
				return nil, fmt.Errorf("auth handler did not return a key")
			}

			return &ssh.Permissions{Extensions: map[string]string{"key-id": pkey.Id}}, nil
		}
	}

	keypath := s.config.KeyPath()
	if !fileExists(keypath) {
		if err := s.createServerKey(); err != nil {
			return err
		}
	}

	privateBytes, err := ioutil.ReadFile(keypath)
	if err != nil {
		return err
	}

	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		return err
	}

	config.AddHostKey(private)
	s.sshconfig = config
	return nil
}

func (s *SSH) ListenAndServe(bind string) error {
	if err := s.setup(); err != nil {
		return err
	}

	if err := s.config.Setup(); err != nil {
		return err
	}

	listener, err := net.Listen("tcp", bind)
	if err != nil {
		return err
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("ssh: error accepting incoming connection: %v", err)
			continue
		}

		go func() {
			log.Printf("ssh: handshaking for %s", conn.RemoteAddr())

			sConn, chans, reqs, err := ssh.NewServerConn(conn, s.sshconfig)
			if err != nil {
				if err == io.EOF {
					log.Printf("ssh: handshaking was terminated: %v", err)
				} else {
					log.Printf("ssh: error on handshaking: %v", err)
				}
				return
			}

			log.Printf("ssh: connection from %s (%s)", sConn.RemoteAddr(), sConn.ClientVersion())

			if s.config.SSHAuth && s.config.GitUser != "" && sConn.User() != s.config.GitUser {
				sConn.Close()
				return
			}

			keyID := ""
			if sConn.Permissions != nil {
				keyID = sConn.Permissions.Extensions["key-id"]
			}

			go ssh.DiscardRequests(reqs)
			go s.handleConnection(keyID, chans)
		}()
	}
}

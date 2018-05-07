package command

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"log"
	"net/url"
	"path"
	"strings"

	"code.cloudfoundry.org/cli/plugin"
	flags "github.com/jessevdk/go-flags"
	uuid "github.com/nu7hatch/gouuid"
)

// Logger is used for outputting log-cache results and errors
type Logger interface {
	Printf(format string, args ...interface{})
	Fatalf(format string, args ...interface{})
	Print(...interface{})
}

type createDrainOpts struct {
	AppOrServiceName string
	AdapterType      string `long:"adapter-type"`
	DrainName        string `long:"drain-name"`
	DrainType        string `long:"type"`
	DrainURL         string
	Username         string `long:"username"`
	Password         string
}

func (f *createDrainOpts) serviceName() string {
	if f.DrainName != "" {
		return f.DrainName
	}

	guid, err := uuid.NewV4()
	if err != nil {
		log.Fatalf("%s", err)
	}

	return fmt.Sprint("cf-drain-", guid)
}

func CreateDrain(
	cli plugin.CliConnection,
	args []string,
	d Downloader,
	p passwordReader,
	log Logger,
) {
	opts := createDrainOpts{
		AdapterType: "service",
	}

	parser := flags.NewParser(&opts, flags.HelpFlag|flags.PassDoubleDash)
	args, err := parser.ParseArgs(args)
	if err != nil {
		log.Fatalf("%s", err)
	}

	if len(args) != 2 {
		log.Fatalf("Invalid arguments, expected 2, got %d.", len(args))
	}

	opts.AppOrServiceName = args[0]
	opts.DrainURL = args[1]

	u, err := url.Parse(opts.DrainURL)
	if err != nil {
		log.Fatalf("Invalid syslog drain URL: %s", err)
	}

	if opts.DrainType != "" {
		if !validDrainType(opts.DrainType) {
			log.Fatalf("Invalid type: %s", opts.DrainType)
		}

		qValues := u.Query()
		qValues.Set("drain-type", opts.DrainType)
		u.RawQuery = qValues.Encode()
	}

	switch opts.AdapterType {
	case "service":
		createAndBindService(cli, u, opts.AppOrServiceName, opts.serviceName(), log)
	case "application":
		pushSyslogForwarder(
			cli,
			u,
			opts.AppOrServiceName,
			opts.serviceName(),
			opts.Username,
			opts.Password,
			d,
			p,
			log,
		)
	default:
		log.Fatalf("unsupported adapter type, must be 'service' or 'application'")
	}
}

func createAndBindService(
	cli plugin.CliConnection,
	u *url.URL,
	appName, serviceName string,
	log Logger,
) {
	_, err := cli.GetApp(appName)
	if err != nil {
		log.Fatalf("%s", err)
	}

	command := []string{"create-user-provided-service", serviceName, "-l", u.String()}
	_, err = cli.CliCommand(command...)
	if err != nil {
		log.Fatalf("%s", err)
	}

	command = []string{"bind-service", appName, serviceName}
	_, err = cli.CliCommand(command...)
	if err != nil {
		log.Fatalf("%s", err)
	}
}

func pushSyslogForwarder(
	cli plugin.CliConnection,
	u *url.URL,
	appOrServiceName string,
	serviceName string,
	username string,
	password string,
	d Downloader,
	p passwordReader,
	log Logger,
) {
	sourceID, err := sourceID(cli, appOrServiceName)
	if err != nil {
		log.Fatalf("unknown application or service %q", appOrServiceName)
	}

	org, err := cli.GetCurrentOrg()
	if err != nil {
		log.Fatalf("%s", err)
	}
	space, err := cli.GetCurrentSpace()
	if err != nil {
		log.Fatalf("%s", err)
	}
	apiEndpoint, err := cli.ApiEndpoint()
	if err != nil {
		log.Fatalf("%s", err)
	}

	if username == "" {
		username = fmt.Sprintf("drain-%s", sourceID)
		password = createUser(cli, username, log)
	}

	if username != "" && password == "" {
		log.Printf("Enter a password for %s: ", username)
		bytePassword, err := p(0)
		if err != nil {
			log.Fatalf("%s", err)
		}

		if string(bytePassword) == "" {
			log.Fatalf("Password cannot be blank.")
		}
		password = string(bytePassword)
	}

	path := path.Dir(d.Download("syslog_forwarder"))

	command := []string{
		"push",
		serviceName,
		"-p", path,
		"-b", "binary_buildpack",
		"-c", "./syslog_forwarder",
		"--no-start",
	}
	_, err = cli.CliCommand(command...)
	if err != nil {
		log.Fatalf("%s", err)
	}

	skipCertVerify, err := cli.IsSSLDisabled()
	if err != nil {
		log.Fatalf("%s", err)
	}

	hostName := fmt.Sprintf("%s.%s.%s", org.Name, space.Name, appOrServiceName)
	uaaAddr := strings.Replace(apiEndpoint, "api.", "uaa.", 1)
	logCacheAddr := strings.Replace(apiEndpoint, "api.", "log-cache.", 1)
	groupName, err := uuid.NewV4()
	if err != nil {
		log.Fatalf("%s", err)
	}
	envCommands := [][]string{
		{"set-env", serviceName, "SOURCE_ID", sourceID},
		{"set-env", serviceName, "SOURCE_HOST_NAME", hostName},
		{"set-env", serviceName, "UAA_URL", uaaAddr},
		{"set-env", serviceName, "CLIENT_ID", "cf"},
		{"set-env", serviceName, "USERNAME", username},
		{"set-env", serviceName, "PASSWORD", password},
		{"set-env", serviceName, "LOG_CACHE_HTTP_ADDR", logCacheAddr},
		{"set-env", serviceName, "SYSLOG_URL", u.String()},
		{"set-env", serviceName, "SKIP_CERT_VERIFY", fmt.Sprintf("%t", skipCertVerify)},
		{"set-env", serviceName, "GROUP_NAME", groupName.String()},
		{"set-env", serviceName, "DRAIN_SCOPE", "single"},
	}

	for _, cmd := range envCommands {
		_, err = cli.CliCommandWithoutTerminalOutput(cmd...)
		if err != nil {
			log.Fatalf("%s", err)
		}
	}

	command = []string{"start", serviceName}
	_, err = cli.CliCommand(command...)
	if err != nil {
		log.Fatalf("%s", err)
	}
}

func sourceID(cli plugin.CliConnection, appOrServiceName string) (string, error) {
	app, err := cli.GetApp(appOrServiceName)
	if err != nil {
		svc, err := cli.GetService(appOrServiceName)
		if err != nil {
			return "", err
		}

		return svc.Guid, nil
	}

	return app.Guid, nil
}

func validDrainType(drainType string) bool {
	switch drainType {
	case "logs", "metrics", "all":
		return true
	default:
		return false
	}
}

func buildDrainName(drainName string) string {
	if drainName != "" {
		return drainName
	}

	guid, err := uuid.NewV4()
	if err != nil {
		log.Fatalf("%s", err)
	}

	return fmt.Sprint("cf-drain-", guid)
}

func createUser(cli plugin.CliConnection, username string, log Logger) string {
	data := make([]byte, 20)
	_, err := rand.Read(data)
	if err != nil {
		log.Fatalf("%s", err)
	}
	password := fmt.Sprintf("%x", sha256.Sum256(data))

	_, err = cli.CliCommand("create-user", username, password)
	if err != nil {
		log.Fatalf("%s", err)
	}

	org, err := cli.GetCurrentOrg()
	if err != nil {
		log.Fatalf("%s", err)
	}

	space, err := cli.GetCurrentSpace()
	if err != nil {
		log.Fatalf("%s", err)
	}

	_, err = cli.CliCommand(
		"set-space-role",
		username,
		org.Name,
		space.Name,
		"SpaceDeveloper",
	)
	if err != nil {
		log.Fatalf("%s", err)
	}

	return password
}

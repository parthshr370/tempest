package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	openaiauth "go.harness.dev/harness/internal/auth/openai"
)

func runLogin(args []string, stdout, stderr io.Writer) int {
	return runOpenAIAuth("login", args, stdout, stderr)
}
func runLogout(args []string, stdout, stderr io.Writer) int {
	return runOpenAIAuth("logout", args, stdout, stderr)
}

func runAuth(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "status" && args[0] != "list" {
		fmt.Fprintln(stderr, "usage: tempest auth status|list [openai] [--profile label]")
		return exitUsage
	}
	return runOpenAIAuth(args[0], args[1:], stdout, stderr)
}

func runOpenAIAuth(command string, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "openai" {
		args = args[1:]
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	profile := flags.String("profile", "", "saved account label (default: active account, or default on first use)")
	noBrowser := flags.Bool("no-browser", false, "print the authorization URL without opening a browser")
	consent := flags.Bool("consent", false, "ask again for ChatGPT plan usage permission")
	issuer := flags.String("issuer", openaiauth.Issuer, "OpenAI authority; a loopback HTTP authority is allowed for local protocol tests")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: tempest "+command+" [openai] [--profile label]")
		return exitUsage
	}
	authManager, err := openaiauth.Default()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	authManager.Issuer = *issuer
	if err := openaiauth.ValidateIssuer(*issuer); err != nil {
		fmt.Fprintln(stderr, err)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch command {
	case "login":
		fmt.Fprintln(stdout, "Continue with ChatGPT")
		status, err := authManager.Login(ctx, *profile, *consent, func(authorizationURL string) error {
			fmt.Fprintln(stdout, "Open this URL in a browser on this host:")
			fmt.Fprintln(stdout, authorizationURL)
			if !*noBrowser {
				if err := openSignInBrowser(ctx, authorizationURL); err != nil {
					fmt.Fprintln(stderr, "Could not open the browser. Open the URL above manually on this host.")
				}
			}
			return nil
		})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		printSignInStatus(stdout, status)
		fmt.Fprintln(stdout, "Using ChatGPT plan with openai-chatgpt:<model>. Plan and app limits apply; authorized credits may be used.")
		fmt.Fprintln(stdout, "Manage usage and access: https://chatgpt.com/settings/usage")
	case "logout":
		if err := authManager.Logout(ctx, *profile); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "Signed out of ChatGPT. Saved account registration and host identity retained.")
	case "status":
		status, err := authManager.Status(*profile)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		printSignInStatus(stdout, status)
	case "list":
		statuses, err := authManager.List()
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if len(statuses) == 0 {
			fmt.Fprintln(stdout, "Not signed in with ChatGPT. Run tempest login openai.")
		}
		for _, status := range statuses {
			printSignInStatus(stdout, status)
		}
	}
	return 0
}

func printSignInStatus(output io.Writer, status openaiauth.Status) {
	if !status.SignedIn {
		fmt.Fprintf(output, "ChatGPT profile %q: signed out; run tempest login openai\n", status.Profile)
		return
	}
	fmt.Fprintf(output, "ChatGPT profile %q: account=%q expires=%s\n", status.Profile, status.Account, status.ExpiresAt.UTC().Format(time.RFC3339))
	if !status.PlanEnabled {
		fmt.Fprintf(output, "ChatGPT plan use is disabled; run tempest login openai --profile %s --consent.\n", status.Profile)
	}
}

func openSignInBrowser(ctx context.Context, authorizationURL string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(ctx, "open", authorizationURL)
	case "windows":
		command = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", authorizationURL)
	default:
		command = exec.CommandContext(ctx, "xdg-open", authorizationURL)
	}
	return command.Run()
}

func printChatGPTDoctor(output io.Writer) {
	authManager, err := openaiauth.Default()
	if err != nil {
		fmt.Fprintln(output, "WARN ChatGPT sign-in: cannot resolve credential directory")
		return
	}
	status, err := authManager.Status("")
	if err != nil {
		fmt.Fprintln(output, "WARN ChatGPT sign-in: cannot read local credentials; run tempest login openai")
		return
	}
	printSignInStatus(output, status)
}

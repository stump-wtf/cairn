package clicmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// readToken resolves the token `cairn login` verifies and stores, in the
// priority cairn#21 specifies: an explicit --token flag, then piped stdin,
// then an interactive no-echo prompt (cairn#21 "accept a token (flag/
// prompt/stdin, never echoed)"). It never echoes a typed token to the
// terminal, never writes it anywhere itself, and returns a usage error
// (exit 2) rather than hanging when none of the three sources produced one.
func readToken(streams IOStreams, flagToken string) (string, error) {
	if flagToken != "" {
		return flagToken, nil
	}

	if !IsTerminal(streams.In) {
		return readTokenFromPipe(streams.In)
	}
	return readTokenFromPrompt(streams)
}

// readTokenFromPipe reads the whole of a piped stdin as the token, trimming
// surrounding whitespace (a trailing newline is the common case for
// `echo "$TOKEN" | cairn login`).
func readTokenFromPipe(in io.Reader) (string, error) {
	br := bufio.NewReader(in)
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		if err == io.EOF {
			return "", usageErrorf("no token provided: pass --token, pipe a token on stdin, or run `cairn login` interactively")
		}
		return "", fmt.Errorf("cairn login: read token from stdin: %w", err)
	}
	tok := strings.TrimSpace(line)
	if tok == "" {
		return "", usageErrorf("no token provided: pass --token, pipe a token on stdin, or run `cairn login` interactively")
	}
	return tok, nil
}

// readTokenFromPrompt prompts on stderr (so a redirected stdout, e.g. into a
// file, is never polluted) and reads the token from the terminal with input
// echo disabled, exactly like an SSH or sudo password prompt.
func readTokenFromPrompt(streams IOStreams) (string, error) {
	f, ok := streams.In.(*os.File)
	if !ok {
		return "", usageErrorf("no token provided: pass --token or pipe a token on stdin")
	}
	fmt.Fprint(streams.ErrOut, "Token: ")
	b, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(streams.ErrOut) // the Enter keypress isn't echoed with input hidden
	if err != nil {
		return "", fmt.Errorf("cairn login: read token: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", usageErrorf("no token provided: pass --token, pipe a token on stdin, or run `cairn login` interactively")
	}
	return tok, nil
}

# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Command-governance classifier used by the command-governance example.

Classifies a command string as denied (destructive `rm`/`mkfs`/`dd`) or
allowed *before* it is dispatched to a running sandbox's execution API, so a
denied command never reaches the pod at all. See README.md for the full
writeup, scope, and rationale.
"""

import os
import re
import shlex

# Fork bombs and pipe-to-shell installers are inherently about shell syntax
# (subshell/pipe metacharacters) rather than a single command's argv, so
# they stay regex-based.
#
# rm/mkfs/dd are ALSO matched here on raw text, as a second layer alongside
# the parsed-argv check in _is_denied_segment. The argv check resolves
# quoting/flag-reordering bypasses (`r\m -rf /`, `rm '-rf' /`, `rm -r -f /`)
# that a raw regex would miss, but it only ever sees a single already-split
# command segment - it can't see through every shape a shell can wrap that
# segment in (env-var prefixes, `sudo`/`env`/`xargs`, subshells `$(...)` /
# backticks, or the command simply appearing as a substring anywhere in the
# line). A raw substring match on the command name and its flags catches
# those without needing to parse each wrapper's own grammar - `\b` word
# boundaries keep `transform -rf foo` benign.
_DESTRUCTIVE_SYNTAX_PATTERNS = [
    r":\(\)\s*\{\s*:\|:&\s*\}\s*;\s*:",  # fork bomb
    r"curl[^|]*\|\s*(sh|bash)\b",
    r"wget[^|]*\|\s*(sh|bash)\b",
    r"\brm\s+-[a-z]*r[a-z]*f|\brm\s+-[a-z]*f[a-z]*r",
    r"\bmkfs\b",
    r"\bdd\b.*\bof=",  # order-independent, mirrors the of=-only check in _is_denied_segment
]

# sudo/xargs/env flags that consume the following token as their own argument
# (not the start of the wrapped command), e.g. `sudo -u root rm -rf /` and
# `sudo --user root rm -rf /` must both resolve to `rm -rf /` rather than
# stopping at the "root" token. Short and long forms are listed separately per
# wrapper since they don't share a flag namespace (xargs's `-n` is unrelated
# to sudo's).
_SUDO_FLAGS_WITH_ARG = {
    "-u", "--user",
    "-g", "--group",
    "-h", "--host",
    "-p", "--prompt",
    "-U", "--other-user",
    "-r", "--role",
    "-t", "--type",
    "-C", "--close-from",
    "-a", "--auth-type",
    "-T", "--command-timeout",
    "-D", "--chdir",
    "-R", "--chroot",
}
# `-I`/`-L` (uppercase) and `--replace`/`--max-lines` require a separate
# following token; `-i`/`-l` (lowercase, deprecated BSD-compatibility forms)
# take an OPTIONAL argument that must be attached to the flag itself
# (`-ifoo`, never `-i foo`) - treating them as separate-token-consuming here
# would eat the wrapped command's own executable as if it were that optional
# value (`xargs -i rm -rf /` would otherwise swallow "rm").
_XARGS_FLAGS_WITH_ARG = {
    "-a", "--arg-file",
    "-d", "--delimiter",
    "-I", "--replace",
    "-L", "--max-lines",
    "-n", "--max-args",
    "-P", "--max-procs",
    "-s", "--max-chars",
    "--process-slot-var",
}
_ENV_FLAGS_WITH_ARG = {"-u", "--unset", "-C", "--chdir", "-S", "--split-string"}
_NICE_FLAGS_WITH_ARG = {"-n", "--adjustment"}
_STDBUF_FLAGS_WITH_ARG = {"-i", "-o", "-e", "--input", "--output", "--error"}
_TIMEOUT_FLAGS_WITH_ARG = {"-k", "--kill-after", "-s", "--signal"}
_EXEC_FLAGS_WITH_ARG = {"-a"}

# Characters that mean a token's real value can't be known statically - shell
# expansion (`$FOO`, `$(...)`, backticks), a glob (`*?[`), or brace expansion
# (`{a,b}`) that the shell resolves at runtime, not parse time. A token in the
# executable position that contains one of these could resolve to anything,
# including `rm --recursive --force` (`$(printf rm) --recursive --force /`,
# `/bin/r[m] -rf /`, `rm$IFS-rf$IFS/` with IFS word-splitting gluing the whole
# invocation into a single token, or `{rm,-rf,/}` where brace expansion turns
# one token into the three words `rm -rf /`). Treated as denied rather than
# evaluated, since evaluating shell expansion correctly means being a shell.
# `}` is included alongside `{` for the same reason, even though only `{`
# starts a real brace expansion: a `}` with no matching `{` in a token this
# code is about to trust as a literal executable name is itself suspicious.
_EXPANSION_CHARS = frozenset("$`*?[{}")


def _looks_expanded(token: str) -> bool:
    return any(c in _EXPANSION_CHARS for c in token)


def _flags(tokens: list[str]) -> list[str]:
    # Tokens up to (not including) a bare "--" end-of-options marker, so
    # `rm -- --recursive --force` (real filenames, not flags) isn't denied.
    if "--" in tokens:
        return tokens[: tokens.index("--")]
    return tokens


def _has_flag(tokens: list[str], short: str, long_name: str) -> bool:
    # Checks parsed argv tokens, not the raw string, so quoting/escaping
    # ("-r -f", "'-rf'") can't hide a flag the shell would still honor.
    for t in tokens:
        tl = t.lower()
        if tl == long_name:
            return True
        # GNU getopt_long accepts any unambiguous abbreviation of a long
        # option (`rm --rec --for /` really is `--recursive --force`).
        # Matching any prefix here - not just rm's actual unambiguous ones -
        # is deliberately over-inclusive: a denylist should err toward
        # denying too much rather than missing a real invocation.
        if tl.startswith("--") and len(tl) > 2 and long_name.startswith(tl):
            return True
        if t.startswith("-") and not t.startswith("--") and short.lower() in t.lower():
            return True
    return False


_ENV_ASSIGNMENT_PATTERN = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")


def _strip_flags_with_arg(tokens: list[str], flags_with_arg: set[str]) -> list[str]:
    while tokens and tokens[0].startswith("-"):
        flag = tokens[0]
        tokens = tokens[1:]
        if flag in flags_with_arg and tokens:
            tokens = tokens[1:]
    return tokens


# env's `-S`/`--split-string` takes a single argument that is itself a shell
# command string subject to further word splitting (`env -S 'rm -rf /'` runs
# `rm -rf /`). Correctly classifying it means recursively parsing a second
# layer of shell grammar; silently discarding it like an opaque flag argument
# (as every other _ENV_FLAGS_WITH_ARG entry is) would let the wrapped command
# bypass classification entirely. So _strip_wrapper fails closed instead - for
# both the separate-token form (`-S 'rm -rf /'`) and GNU's attached forms
# (`-S'rm -rf /'`, `--split-string='rm -rf /'`), which shlex merges into a
# single token that doesn't equal "-S"/"--split-string" outright.
_ENV_SPLIT_STRING_FLAGS = {"-S", "--split-string"}


def _is_env_split_string_flag(token: str) -> bool:
    return (
        token in _ENV_SPLIT_STRING_FLAGS
        or token.startswith("-S")
        or token.startswith("--split-string=")
    )


def _strip_wrapper(tokens: list[str]) -> tuple[list[str], bool] | None:
    """Strips `sudo`/`env`/`xargs`/`command`/`nohup`/`nice`/`stdbuf`/
    `timeout`/`exec` wrapper prefixes (and their own flags or env-var
    assignments) so e.g. `sudo rm -rf /` and `env rm -rf /` resolve to the
    same argv as `rm -rf /` for the checks below.

    Executable names are matched case-insensitively (`SUDO`, `Env`, ...), and
    a bare `VAR=val` prefix is stripped even without a leading `env` keyword,
    since a shell honors `FOO=bar rm -rf /` the same as `env FOO=bar rm -rf /`.
    Each wrapper's own arg-taking flags (both short and long forms, e.g.
    `-u`/`--user` for sudo) are consumed by their own flag set - an
    unrecognized wrapper flag is assumed to take no argument, which is only
    safe because every flag actually documented for these wrappers is listed
    below; a wrapper flag missing from these sets could still leak its
    argument through as the apparent executable.

    Returns `(resolved_tokens, used_xargs)` - `used_xargs` is True if an
    `xargs` layer was stripped anywhere in the chain, since unlike every other
    wrapper here, xargs appends arguments read from its *stdin* (a separate,
    unseen pipeline segment) to the wrapped command; the caller can't trust an
    absence of visible `-rf`/`of=` on the xargs line to mean the executed
    command lacks them.

    Returns None (denied) for `env -S`/`--split-string`, an unsupported form
    - see `_ENV_SPLIT_STRING_FLAGS`.
    """
    used_xargs = False
    while tokens:
        if _ENV_ASSIGNMENT_PATTERN.match(tokens[0]):
            tokens = tokens[1:]
            continue
        exe = os.path.basename(tokens[0]).lower()
        if exe == "sudo":
            tokens = _strip_flags_with_arg(tokens[1:], _SUDO_FLAGS_WITH_ARG)
            continue
        if exe == "xargs":
            used_xargs = True
            tokens = _strip_flags_with_arg(tokens[1:], _XARGS_FLAGS_WITH_ARG)
            continue
        if exe == "env":
            tokens = tokens[1:]
            while tokens and (
                tokens[0].startswith("-") or _ENV_ASSIGNMENT_PATTERN.match(tokens[0])
            ):
                if _is_env_split_string_flag(tokens[0]):
                    return None
                if tokens[0] in _ENV_FLAGS_WITH_ARG:
                    tokens = tokens[2:] if len(tokens) > 1 else tokens[1:]
                else:
                    tokens = tokens[1:]
            continue
        if exe == "command":
            # bash builtin: `command rm -rf /` (optionally `command -p`/`-v`/
            # `-V`, none of which take an argument) still just runs the
            # wrapped command.
            tokens = tokens[1:]
            while tokens and tokens[0] in ("-p", "-v", "-V"):
                tokens = tokens[1:]
            continue
        if exe == "nohup":
            # no flags relevant to the wrapped command's identity.
            tokens = tokens[1:]
            continue
        if exe == "nice":
            tokens = _strip_flags_with_arg(tokens[1:], _NICE_FLAGS_WITH_ARG)
            continue
        if exe == "stdbuf":
            tokens = _strip_flags_with_arg(tokens[1:], _STDBUF_FLAGS_WITH_ARG)
            continue
        if exe == "timeout":
            # `timeout [OPTION] DURATION COMMAND [ARG]...` - unlike the other
            # launchers here, timeout also has a mandatory positional
            # DURATION between its own flags and the wrapped command, which
            # must be stripped too or it'd be mistaken for the executable.
            tokens = _strip_flags_with_arg(tokens[1:], _TIMEOUT_FLAGS_WITH_ARG)
            if tokens:
                tokens = tokens[1:]
            continue
        if exe == "exec":
            tokens = _strip_flags_with_arg(tokens[1:], _EXEC_FLAGS_WITH_ARG)
            continue
        break
    return tokens, used_xargs


def _is_denied_segment(tokens: list[str]) -> bool:
    """Token-aware rm/mkfs/dd check for a single (already-split) command
    segment.

    Uses shlex-parsed tokens so quoting/escaping that a POSIX shell would
    resolve to a plain `rm -rf` (or `mkfs`, `dd`) can't hide the command
    from a raw-string regex.
    """
    stripped = _strip_wrapper(tokens)
    if stripped is None:
        return True  # unsupported wrapper form (e.g. `env -S ...`) - fail safe, deny
    tokens, used_xargs = stripped
    if not tokens:
        return False
    if _looks_expanded(tokens[0]):
        return True  # exe resolves via shell expansion/glob - can't statically know it, deny
    exe = os.path.basename(tokens[0]).lower()  # strips a path prefix like /bin/rm
    is_destructive_exe = exe in ("rm", "dd") or exe == "mkfs" or exe.startswith("mkfs.")
    if used_xargs and is_destructive_exe:
        # xargs appends argv built from its stdin - a `-rf`/`of=` invisible on
        # this line can still arrive at runtime, so any visible-flags check
        # below can't be trusted to say "safe" for one of these commands.
        return True
    if is_destructive_exe and any(_looks_expanded(t) for t in tokens[1:]):
        # An argument (not just the executable) that resolves via shell
        # expansion could itself be, or contain, a destructive flag -
        # `FLAGS=-rf; rm $FLAGS /` and `X=ve; rm --recursi$X --force /` both
        # pass the literal flag check below yet execute `rm -rf /`.
        return True
    args = _flags(tokens[1:])
    if exe == "rm":
        return _has_flag(args, "r", "--recursive") and _has_flag(args, "f", "--force")
    if exe == "mkfs" or exe.startswith("mkfs."):
        return True
    if exe == "dd":
        return any(t.startswith("of=") for t in tokens[1:])  # order-independent
    return False


def _line_segments(line: str) -> list[list[str]] | None:
    """Tokenizes a shell line and splits it into command segments on `;`,
    `&&`, `||`, `&` and `|`, so each chained/piped command is checked on its
    own instead of only the first one on the line. Returns None if the line
    can't be tokenized (fail-safe: caller treats it as denied).
    """
    try:
        lexer = shlex.shlex(line, posix=True, punctuation_chars="|&;")
        lexer.whitespace_split = True
        tokens = list(lexer)
    except ValueError:
        return None
    segments: list[list[str]] = []
    current: list[str] = []
    for tok in tokens:
        if tok and set(tok) <= set("|&;"):
            if current:
                segments.append(current)
            current = []
        else:
            current.append(tok)
    if current:
        segments.append(current)
    return segments


def _extract_subshells(text: str) -> list[str] | None:
    """Extracts the contents of every `$(...)` and backtick-quoted command
    substitution in `text`, recursing into each one so a substitution nested
    inside another (`$(rm -rf $(pwd))`) is also pulled out - a fixed regex
    can't do this because it isn't recursive: it can't match an outer `$(...)`
    whose own body contains another `$(...)`, so the outer command would
    never get extracted as its own segment and would slip past
    unclassified. Tracks paren/backtick depth by hand instead.

    Returns None if `text` contains an unterminated `$(` or backtick, so the
    caller fails closed rather than silently skipping unparsed content.
    """
    found: list[str] = []
    i, n = 0, len(text)
    while i < n:
        if text[i] == "`":
            end = text.find("`", i + 1)
            if end == -1:
                return None
            inner = text[i + 1 : end]
            i = end + 1
        elif text[i : i + 2] == "$(":
            depth = 1
            j = i + 2
            while j < n and depth:
                if text[j] == "(":
                    depth += 1
                elif text[j] == ")":
                    depth -= 1
                j += 1
            if depth:
                return None
            inner = text[i + 2 : j - 1]
            i = j
        else:
            i += 1
            continue
        found.append(inner)
        nested = _extract_subshells(inner)
        if nested is None:
            return None
        found.extend(nested)
    return found


def is_denied(command: str) -> bool:
    """Returns True if `command` should be denied before it reaches a
    sandbox's execution API.

    Checks the raw text for destructive syntax first, then splits the
    command on shell chaining/piping metacharacters and command
    substitution, and checks each resulting segment's parsed argv
    independently — so `echo hi && rm -rf /`, `$(rm -rf /)`, and
    `sudo rm -rf /` are all caught, not just a bare `rm -rf /`.
    """
    # POSIX line continuation: an unquoted backslash immediately followed by
    # a newline is removed entirely by the shell, joining the next line onto
    # the current one with no inserted whitespace - so `rm \` + newline +
    # `'-rf' /` really is one command, `rm '-rf' /`, not two separately
    # benign lines. Stripped before both the raw-text check and the
    # line-splitting below see the command, so neither is fooled by a
    # continuation into treating one destructive command as two safe halves.
    command = re.sub(r"\\\n", "", command)
    if any(re.search(p, command, re.IGNORECASE) for p in _DESTRUCTIVE_SYNTAX_PATTERNS):
        return True
    lines = list(command.splitlines())
    for line in list(lines):
        subshells = _extract_subshells(line)  # also check inside $(...) / `...`
        if subshells is None:
            return True  # unterminated $(...)/`...` - fail safe, deny
        lines.extend(subshells)
    for line in lines:
        segments = _line_segments(line)
        if segments is None:
            return True  # unparseable quoting - fail safe, deny
        if any(_is_denied_segment(segment) for segment in segments):
            return True
    return False


def governed_run(sandbox, command: str):
    if is_denied(command):
        raise PermissionError(f"denied by command policy: {command}")
    return sandbox.commands.run(command)

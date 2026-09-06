#!/bin/sh
# Sandbox session test fixture: a line-oriented echo server that runs inside
# the confined session child. Each request line is either echoed back as one
# line, answered with an environment value, or handled by a directive that
# simulates misbehaving children (slow responses, oversized responses,
# stderr chatter, crash).
#
# Directives (exact line):
#   @echo off        stop echoing; drain stdin silently
#   @env <KEY>       answer with the value of environment variable KEY
#   @slow <dur>      answer the NEXT request after sleeping dur
#   @slow-forever    stop answering entirely
#   @big <n>         answer the NEXT request with n bytes plus newline
#   @noise <n>       write n bytes to stderr immediately, then resume
#   @crash           exit immediately without answering pending requests
#
# The fixture must stay a plain interpreter script using only shell builtins
# and coreutils: the confined child runs under the seccomp syscall allowlist,
# and a runtime that issues denied syscalls (bash calls socket at startup,
# a Go binary calls prctl) is killed before the protocol starts.
echo_on=1
delay=0
big=-1
while IFS= read -r line; do
  case "$line" in
    "@echo off")
      echo_on=0
      printf 'ok\n'
      ;;
    "@slow-forever")
      echo_on=0
      printf 'ok\n'
      ;;
    "@slow "*)
      delay="${line#@slow }"
      printf 'ok\n'
      ;;
    "@big "*)
      big="${line#@big }"
      printf 'ok\n'
      ;;
    "@noise "*)
      n="${line#@noise }"
      printf 'n%.0s' $(seq 1 "$n") >&2
      printf 'ok\n'
      ;;
    "@crash")
      exit 9
      ;;
    "@env "*)
      key="${line#@env }"
      printf '%s\n' "$(printenv "$key")"
      ;;
    *)
      if [ "$echo_on" = 1 ]; then
        if [ "$delay" != "0" ]; then
          sleep "$delay"
          delay=0
        fi
        # No redirection to /dev/null here: the confined child's Landlock
        # roots do not include /dev, and a failed redirection would abort
        # the branch instead of testing it.
        if [ "$big" -ge 0 ]; then
          printf 'y%.0s' $(seq 1 "$big")
          printf '\n'
          big=-1
        else
          printf '%s\n' "$line"
        fi
      fi
      ;;
  esac
done

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
#   @connect         spawn bash as a connector subprocess (real socket
#                    syscall via /dev/tcp); answer allowed/denied-<status>.
#                    bash itself is killed by the seccomp filter the moment
#                    it issues socket(); this POSIX-sh loop survives, reads
#                    the connector's exit status, and reports it, so the
#                    denial is observable from inside the session child.
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
    "@connect")
      # AC-02 probe: bash's /dev/tcp redirection issues a real socket()
      # syscall; seccomp (KILL_PROCESS) answers with SIGSYS — exit status
      # 159 = 128+SIGSYS(31) — and no connection can exist. The session
      # child itself survives and reports the status. The connector's own
      # redirections go to a file in the working directory, NOT /dev/null:
      # the child's Landlock roots exclude /dev and its filter denies syscalls
      # a redirect to a character device would need. Plain bash is required
      # (dash has no /dev/tcp and fails with a plain open error).
      bash -c 'exec 3<>/dev/tcp/203.0.113.1/1' > ./connect-out.txt 2>&1
      status=$?
      /usr/bin/rm -f ./connect-out.txt
      printf 'denied-%s\n' "$status"
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

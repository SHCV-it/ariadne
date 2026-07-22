# ariadne.bash — shell integration (bash port of ariadne.zsh)
#
# Bash has no ZLE: no widgets, no POSTDISPLAY, no zsh/net/socket. The zsh
# design is reproduced here on three readline/bash mechanisms:
#
#   transport   bash cannot open unix sockets. A single persistent coproc
#               (socat, or OpenBSD nc -U, or ncat) bridges the daemon socket.
#               Fork-per-keystroke is still avoided; the bridge is spawned
#               once per shell session, exactly like zsocket's fd.
#   keystrokes  every printable ASCII char plus Backspace/Delete/C-w/C-k is
#               bound via `bind -x` to a function that splices READLINE_LINE.
#               Empirically (bash 5.x): readline clears the line *before* the
#               handler runs and force-redisplays *after* it, starting at
#               wherever the cursor was left. A handler that homes the cursor
#               (\r) may therefore paint anywhere past prompt+buffer and the
#               paint survives the redisplay. That paint *is* the ghost text
#               and the panel — there is no POSTDISPLAY, we print dim bytes.
#   accept hook bind -x handlers do not run from keyboard-macro pushback, so
#               Enter cannot be chained cleanup→accept-line. PS0 (bash 4.4+)
#               is the cleanup point instead: it expands after the line is
#               accepted and before it runs, which is where the accepted line
#               gets repainted without the trailing ghost and the panel rows
#               are reclaimed. Rejection feedback is computed in precmd from
#               the same state, so Enter itself stays 100% native.
#
# Design invariants, identical to the zsh version and in the same order:
#   1. Never block the prompt. Every socket read has a hard timeout and every
#      failure falls through to stock bash behaviour.
#   2. Never shadow a live completer. Tab is rebound to Ariadne only while
#      the daemon claims the token; the moment it doesn't, Tab is restored
#      to the user's original binding, byte for byte.
#   3. Never spawn a process per keystroke. Handlers are builtins + string
#               surgery; the only persistent helper is the socket bridge.
#
# Known deltas vs the zsh version (honest list):
#   * emacs keymap only. All bindings go into the emacs keymap explicitly, so
#     `set -o vi` users get ingest (history learning) but no live layer.
#   * non-ASCII input, bracketed paste, yank, and unbound editing keys do not
#     trigger a re-query; the display heals on the next bound keystroke.
#   * cursor offsets sent to the daemon are character counts; for pure-ASCII
#     buffers this equals the byte offsets the daemon slices by.
#   * numeric arguments and overwrite mode are ignored by the insert handler.
#   * multi-line prompts or multi-line buffers disable painting (query + Tab
#     still work); PS0 reprint is skipped for buffers with control chars.

[[ $- == *i* ]] || return 0
[[ -n ${ARIADNE_SHELL_LOADED:-} ]] && return 0

(( BASH_VERSINFO[0] >= 5 )) || {
  echo "ariadne: bash >= 5.0 required (EPOCHREALTIME, coproc); integration disabled" >&2
  return 0
}

# ---------------------------------------------------------------- config

: ${ARIADNE_SOCKET:=${XDG_RUNTIME_DIR:-/tmp}/ariadne.sock}
: ${ARIADNE_TIMEOUT:=0.02}        # 20ms hard deadline, per the latency budget
: ${ARIADNE_PANEL_LINES:=3}       # 0 disables the panel, leaving ghost text
: ${ARIADNE_GHOST:=1}
: ${ARIADNE_MIN_CHARS:=1}
: ${ARIADNE_COLOR:=1}
: ${ARIADNE_BREAKER_TRIP:=5}      # consecutive timeouts before self-disable
: ${ARIADNE_BREAKER_COOLDOWN:=300}

export ARIADNE_SHELL_LOADED=1

shopt -s extglob                  # C-w word-rubout patterns; harmless otherwise

# The bridge is the one external process this file needs. socat is preferred;
# OpenBSD nc (-U) and ncat cover the common distros. Probe once, at load.
_ARIADNE_BRIDGE=""
if command -v socat >/dev/null 2>&1; then
  _ARIADNE_BRIDGE=socat
elif command -v nc >/dev/null 2>&1 && nc -h 2>&1 | grep -qw -- '-U'; then
  _ARIADNE_BRIDGE=nc
elif command -v ncat >/dev/null 2>&1; then
  _ARIADNE_BRIDGE=ncat
else
  echo "ariadne: no socket bridge found (socat, openbsd nc, or ncat); integration disabled" >&2
  return 0
fi

typeset -g  _ARIADNE_RFD=0 _ARIADNE_WFD=0 _ARIADNE_NC_PID=""
typeset -g  _ARIADNE_LAST_CONNECT_TRY=0
typeset -g  _ARIADNE_SESSION="${RANDOM}${RANDOM}-$$"
typeset -g  _ARIADNE_HOST="${HOSTNAME:-$(hostname 2>/dev/null)}"
typeset -g  _ARIADNE_REPO="" _ARIADNE_BRANCH="" _ARIADNE_LAST_PWD=""
typeset -g  _ARIADNE_FAILS=0 _ARIADNE_DISABLED_UNTIL=0 _ARIADNE_NOTICE=0
typeset -g  _ARIADNE_OWNS=0 _ARIADNE_GHOST=""
typeset -ga _ARIADNE_CAND=() _ARIADNE_DISP=() _ARIADNE_DESC=() _ARIADNE_META=()
typeset -g  _ARIADNE_SHOWN=0 _ARIADNE_CYCLE=0
typeset -g  _ARIADNE_PAINTED=0 _ARIADNE_RESERVED=0
typeset -g  _ARIADNE_PLEN=0 _ARIADNE_MULTILINE=0
typeset -g  _ARIADNE_LAST_BUF="" _ARIADNE_T0="" _ARIADNE_ARMED=0 _ARIADNE_PC_CODE=0
typeset -g  _ARIADNE_IN_PC=0
typeset -g  _ARIADNE_TAB_STATE=0 _ARIADNE_ORIG_TAB=""
typeset -g  _ARIADNE_U=""       # _ariadne_unesc result (avoids a fork per field)

# ---------------------------------------------------------------- transport

_ariadne_enabled() {
  (( _ARIADNE_DISABLED_UNTIL > EPOCHSECONDS )) && return 1
  return 0
}

_ariadne_connect() {
  if (( _ARIADNE_RFD > 0 )) && [[ -n $_ARIADNE_NC_PID ]] && kill -0 "$_ARIADNE_NC_PID" 2>/dev/null; then
    return 0
  fi
  # A dead bridge must not turn into a fork-per-keystroke. Retry at most
  # once every 2s; the breaker covers the gaps in between.
  (( EPOCHSECONDS - _ARIADNE_LAST_CONNECT_TRY < 2 )) && return 1
  _ARIADNE_LAST_CONNECT_TRY=$EPOCHSECONDS
  _ariadne_drop
  [[ -S $ARIADNE_SOCKET ]] || return 1
  # Interactive bash announces every coprocess ("[1] 19349") and its death
  # on the terminal. The stderr redirect suppresses the creation notice;
  # disown prevents the later Done/Exit notices. The bridge's own pipes are
  # unaffected — only the job-table noise is silenced.
  case $_ARIADNE_BRIDGE in
    socat) { coproc ARIADNE_NC { socat - "UNIX-CONNECT:$ARIADNE_SOCKET" 2>/dev/null; } ; } 2>/dev/null ;;
    ncat)  { coproc ARIADNE_NC { ncat --unix "$ARIADNE_SOCKET" 2>/dev/null; } ; } 2>/dev/null ;;
    *)     { coproc ARIADNE_NC { nc -N -U "$ARIADNE_SOCKET" 2>/dev/null; } ; } 2>/dev/null ;;
  esac
  disown 2>/dev/null
  _ARIADNE_RFD=${ARIADNE_NC[0]}
  _ARIADNE_WFD=${ARIADNE_NC[1]}
  _ARIADNE_NC_PID=$ARIADNE_NC_PID
  return 0
}

_ariadne_drop() {
  if (( _ARIADNE_RFD > 0 )); then
    exec {_ARIADNE_RFD}<&- 2>/dev/null
    exec {_ARIADNE_WFD}>&- 2>/dev/null
  fi
  [[ -n $_ARIADNE_NC_PID ]] && kill "$_ARIADNE_NC_PID" 2>/dev/null
  _ARIADNE_RFD=0 _ARIADNE_WFD=0 _ARIADNE_NC_PID=""
}

# Trip the breaker after repeated timeouts. A completion system that
# intermittently stalls the prompt gets uninstalled within a week; better to
# disable ourselves loudly than to degrade quietly. The loud part happens in
# precmd — a bind -x handler's output would be eaten by the redisplay.
_ariadne_fail() {
  _ariadne_drop
  (( _ARIADNE_FAILS++ ))
  if (( _ARIADNE_FAILS >= ARIADNE_BREAKER_TRIP )); then
    _ARIADNE_DISABLED_UNTIL=$(( EPOCHSECONDS + ARIADNE_BREAKER_COOLDOWN ))
    _ARIADNE_FAILS=0
    _ARIADNE_NOTICE=1
  fi
  return 1
}

_ariadne_esc() {
  local s=$1
  s=${s//\\/\\\\}
  s=${s//$'\t'/\\t}
  s=${s//$'\n'/\\n}
  printf '%s' "$s"
}

# Unescape into $_ARIADNE_U. Command substitution would fork once per field
# per candidate — on a keystroke budget that is the whole budget. The fast
# path covers the overwhelming majority: no backslash, no work.
_ariadne_unesc() {
  local s=$1
  if [[ $s != *'\'* ]]; then
    _ARIADNE_U=$s
    return
  fi
  local out="" c n
  local -i i=0 len=${#s}
  while (( i < len )); do
    c=${s:i:1}
    if [[ $c == '\' ]] && (( i + 1 < len )); then
      n=${s:i+1:1}
      case $n in
        t)   out+=$'\t' ;;
        n)   out+=$'\n' ;;
        '\') out+='\' ;;
        *)   out+=$n ;;
      esac
      (( i += 2 ))
    else
      out+=$c
      (( i++ ))
    fi
  done
  _ARIADNE_U=$out
}

# _ariadne_send <line> — write a request, read the response into globals.
# Any read failure drops the bridge: a half-consumed response would desync
# the framing for every subsequent request on the same connection.
_ariadne_send() {
  _ariadne_enabled || return 1
  _ariadne_connect || return 1

  printf '%s\n' "$1" >&$_ARIADNE_WFD 2>/dev/null || { _ariadne_fail; return 1; }

  _ARIADNE_OWNS=0 _ARIADNE_GHOST=""
  _ARIADNE_CAND=() _ARIADNE_DISP=() _ARIADNE_DESC=() _ARIADNE_META=()

  local line
  local -a f
  local -i guard=0
  while true; do
    if ! IFS= read -r -t "$ARIADNE_TIMEOUT" -u "$_ARIADNE_RFD" line 2>/dev/null; then
      _ariadne_fail
      return 1
    fi
    [[ $line == "." ]] && break
    (( ++guard > 24 )) && break
    IFS=$'\t' read -ra f <<< "$line"
    case ${f[0]} in
      OWNS)  _ARIADNE_OWNS=${f[1]:-0} ;;
      GHOST)
        _ariadne_unesc "${f[1]-}"
        _ARIADNE_GHOST=$_ARIADNE_U
        ;;
      CAND)
        _ariadne_unesc "${f[1]-}"; _ARIADNE_CAND+=("$_ARIADNE_U")
        _ariadne_unesc "${f[2]-}"; _ARIADNE_DISP+=("$_ARIADNE_U")
        _ariadne_unesc "${f[3]-}"; _ARIADNE_DESC+=("$_ARIADNE_U")
        _ARIADNE_META+=("${f[4]-0}|${f[5]-}|${f[6]-}")
        ;;
      ERR)   return 1 ;;
    esac
  done
  _ARIADNE_FAILS=0
  return 0
}

# ---------------------------------------------------------------- context

# git context is resolved on directory change only, and without forking git:
# .git/HEAD is one read of a tiny file. Running `git rev-parse` per prompt
# would be a fork per prompt; per keystroke it would be the entire budget.
_ariadne_read_head() { # $1 = gitdir
  local head
  [[ -f $1/HEAD ]] || return 0
  IFS= read -r head < "$1/HEAD" || return 0
  _ARIADNE_BRANCH=${head##*/}   # "ref: refs/heads/main" → main; detached → sha
}

_ariadne_chpwd() {
  [[ $PWD == "$_ARIADNE_LAST_PWD" ]] && return 0
  _ARIADNE_LAST_PWD=$PWD
  _ARIADNE_REPO="" _ARIADNE_BRANCH=""
  local d=$PWD gd
  while [[ -n $d && $d != / ]]; do
    if [[ -d $d/.git ]]; then
      _ARIADNE_REPO=$d
      _ariadne_read_head "$d/.git"
      break
    elif [[ -f $d/.git ]]; then
      # worktree: .git is a file pointing at the real gitdir
      IFS= read -r gd < "$d/.git"
      gd=${gd#gitdir: }
      [[ $gd != /* ]] && gd=$d/$gd
      _ARIADNE_REPO=$d
      _ariadne_read_head "$gd"
      break
    fi
    d=${d%/*}
  done
}

# ---------------------------------------------------------------- query

_ariadne_query() {
  local buf=$READLINE_LINE
  local -i cur=$READLINE_POINT

  if (( ${#buf} < ARIADNE_MIN_CHARS )) && [[ -n $buf ]]; then
    _ARIADNE_CAND=()
    return 1
  fi

  local req="QUERY"
  req+=$'\t'"buf=$(_ariadne_esc "$buf")"
  req+=$'\t'"cur=$cur"
  req+=$'\t'"cwd=$(_ariadne_esc "$PWD")"
  req+=$'\t'"repo=$(_ariadne_esc "$_ARIADNE_REPO")"
  req+=$'\t'"branch=$(_ariadne_esc "$_ARIADNE_BRANCH")"
  req+=$'\t'"host=$(_ariadne_esc "$_ARIADNE_HOST")"
  req+=$'\t'"sess=$_ARIADNE_SESSION"
  req+=$'\t'"n=$ARIADNE_PANEL_LINES"

  _ariadne_send "$req"
}

# ---------------------------------------------------------------- rendering
#
# The model, verified against bash 5.x readline behaviour:
#   * before a bind -x handler runs, readline emits \r\e[K — the input row is
#     blank, cursor at column 0. Old paint on that row is already gone.
#   * after the handler returns, readline reprints prompt+buffer starting at
#     the current cursor position, with no trailing clear-to-EOL.
# So: erase our panel rows, reserve fresh rows below (printing newlines is
# safe — readline addresses everything relatively), paint ghost past the end
# of buffer, paint panel below, leave the cursor at column 0.

# Visible prompt length, computed once per prompt. RL non-printing markers
# (\x01..\x02 spans, from \[ \]), CSI and OSC sequences don't occupy columns.
_ariadne_measure_prompt() {
  _ARIADNE_PLEN=0 _ARIADNE_MULTILINE=0
  local p=${PS1@P} pre rest before
  [[ $p == *$'\n'* ]] && { _ARIADNE_MULTILINE=1; return 0; }
  while [[ $p == *$'\x01'* ]]; do
    before=$p
    pre=${p%%$'\x01'*}; rest=${p#*$'\x01'}; rest=${rest#*$'\x02'}
    p=$pre$rest
    [[ $p == "$before" ]] && break
  done
  local csi_re=$'\e\\[[0-9;?]*[A-Za-z]'
  while [[ $p =~ $csi_re ]]; do
    before=$p
    p=${p/"${BASH_REMATCH[0]}"/}
    [[ $p == "$before" ]] && break
  done
  while [[ $p == *$'\e]'* ]]; do
    before=$p
    pre=${p%%$'\e]'*}; rest=${p#*$'\e]'}
    if [[ $rest == *$'\a'* ]]; then rest=${rest#*$'\a'}
    elif [[ $rest == *$'\e\\'* ]]; then rest=${rest#*$'\e\\'}
    fi
    p=$pre$rest
    [[ $p == "$before" ]] && break
  done
  _ARIADNE_PLEN=${#p}
}

_ariadne_can_paint() {
  (( _ARIADNE_MULTILINE )) && return 1
  [[ $READLINE_LINE == *$'\n'* ]] && return 1
  (( _ARIADNE_PLEN + ${#READLINE_LINE} < COLUMNS - 1 )) || return 1
  return 0
}

_ariadne_paint() {
  local out="" i
  # 1. erase panel rows from the previous keystroke; the input row itself
  #    was already blanked by readline's pre-handler clear.
  if (( _ARIADNE_RESERVED > 0 )); then
    out+=$'\e7'
    for (( i = 1; i <= _ARIADNE_RESERVED; i++ )); do
      out+=$'\e[1B\r\e[K'
    done
    out+=$'\e8'
  fi

  _ariadne_can_paint || { [[ -n $out ]] && printf '%s' "$out"; return 0; }

  local -i rows=${#_ARIADNE_CAND[@]}
  (( rows > ARIADNE_PANEL_LINES )) && rows=$ARIADNE_PANEL_LINES
  (( ARIADNE_PANEL_LINES > 0 )) || rows=0

  # 2. reserve new rows below on first growth. The newlines scroll the
  #    terminal if needed; readline doesn't track absolute rows, so moving
  #    the input line down this way is invisible to it.
  if (( rows > _ARIADNE_RESERVED )); then
    local -i diff=$(( rows - _ARIADNE_RESERVED ))
    for (( i = 0; i < diff; i++ )); do out+=$'\n'; done
    out+=$'\e['"${diff}A"
    _ARIADNE_RESERVED=$rows
  fi

  local dim="" reset="" cyan=""
  if (( ARIADNE_COLOR )); then
    dim=$'\e[2m'; reset=$'\e[0m'; cyan=$'\e[36m'
  fi

  # 3. ghost, past the end of prompt+buffer, truncated to the physical row.
  if (( ARIADNE_GHOST )) && [[ -n $_ARIADNE_GHOST ]] &&
     (( READLINE_POINT == ${#READLINE_LINE} )); then
    local -i col=$(( _ARIADNE_PLEN + ${#READLINE_LINE} ))
    local -i avail=$(( COLUMNS - 1 - col ))
    if (( avail > 0 )); then
      out+=$'\e['"${col}C"$'\e[90m'"${_ARIADNE_GHOST:0:avail}"$'\e[0m'$'\r'
      _ARIADNE_PAINTED=1
    fi
  fi

  # 4. panel rows below the input line.
  if (( rows > 0 )); then
    local marker line tail count ctx src
    out+=$'\e7'
    for (( i = 1; i <= rows; i++ )); do
      IFS='|' read -r count ctx src <<< "${_ARIADNE_META[i-1]}"
      marker="  "
      (( i == _ARIADNE_CYCLE )) && marker="${cyan}▸${reset} "
      line="${marker}${dim}${i}${reset} ${_ARIADNE_DISP[i-1]}"
      tail="${dim}"
      [[ $count != 0 && -n $count ]] && tail+=" ${count}×"
      [[ -n $ctx ]] && tail+=" ${ctx}"
      [[ $src != hist && -n $src ]] && tail+=" [${src}]"
      tail+="${reset}"
      [[ -n ${_ARIADNE_DESC[i-1]} ]] && line+="  ${dim}— ${_ARIADNE_DESC[i-1]}${reset}"
      # never paint past the right edge: a wrapped row would corrupt every
      # subsequent relative cursor move. Truncation may split an SGR
      # sequence, so each row is closed with reset explicitly.
      line=${line:0:$(( COLUMNS - 1 ))}
      out+=$'\e[1B\r'"$line"$'\e[0m\r'
    done
    out+=$'\e8'
    _ARIADNE_PAINTED=1
    _ARIADNE_SHOWN=1
  fi

  [[ -n $out ]] && printf '%s' "$out"
  return 0
}

_ariadne_clear_state() {
  _ARIADNE_OWNS=0 _ARIADNE_GHOST=""
  _ARIADNE_CAND=() _ARIADNE_DISP=() _ARIADNE_DESC=() _ARIADNE_META=()
  _ARIADNE_CYCLE=0
}

# ---------------------------------------------------------------- Tab
#
# The ownership check is load-bearing, same as zsh — but bash cannot fall
# through from inside a bind -x handler, so the decision is made one
# keystroke early: every edit re-evaluates ownership and swaps the Tab
# binding. When the daemon doesn't own the token, Tab *is* the user's
# original binding, not an emulation of it.

_ariadne_set_tab() {
  local -i want=0
  (( _ARIADNE_OWNS )) && (( ${#_ARIADNE_CAND[@]} > 0 )) && want=1
  (( want == _ARIADNE_TAB_STATE )) && return 0
  _ARIADNE_TAB_STATE=$want
  if (( want )); then
    bind -m emacs -x '"\C-i": _ariadne_tab' 2>/dev/null
  elif [[ -n $_ARIADNE_ORIG_TAB ]]; then
    bind -m emacs "$_ARIADNE_ORIG_TAB" 2>/dev/null
  else
    bind -m emacs '"\C-i": complete' 2>/dev/null
  fi
}

_ariadne_tab() {
  (( ${#_ARIADNE_CAND[@]} > 0 )) || { _ariadne_set_tab; return 0; }
  (( _ARIADNE_CYCLE++ ))
  (( _ARIADNE_CYCLE > ${#_ARIADNE_CAND[@]} )) && _ARIADNE_CYCLE=1
  READLINE_LINE=${_ARIADNE_CAND[_ARIADNE_CYCLE-1]}
  READLINE_POINT=${#READLINE_LINE}
  _ARIADNE_GHOST=""
  _ARIADNE_LAST_BUF=$READLINE_LINE
  _ariadne_paint
}

# ---------------------------------------------------------------- widgets

_ariadne_after_edit() {
  _ARIADNE_LAST_BUF=$READLINE_LINE
  _ARIADNE_CYCLE=0
  if _ariadne_query; then
    _ariadne_set_tab
  else
    _ARIADNE_OWNS=0
    _ARIADNE_CAND=()
    _ariadne_set_tab
  fi
  _ariadne_paint
}

_ariadne_insert() {
  local c=$1
  READLINE_LINE="${READLINE_LINE:0:READLINE_POINT}${c}${READLINE_LINE:READLINE_POINT}"
  (( READLINE_POINT += 1 ))
  _ariadne_after_edit
}

_ariadne_backward_delete_char() {
  (( READLINE_POINT == 0 )) && return 0
  READLINE_LINE="${READLINE_LINE:0:READLINE_POINT-1}${READLINE_LINE:READLINE_POINT}"
  (( READLINE_POINT -= 1 ))
  _ariadne_after_edit
}

_ariadne_delete_char() {
  (( READLINE_POINT >= ${#READLINE_LINE} )) && return 0
  READLINE_LINE="${READLINE_LINE:0:READLINE_POINT}${READLINE_LINE:READLINE_POINT+1}"
  _ariadne_after_edit
}

# C-w is unix-word-rubout in emacs mode: kill the previous whitespace-
# delimited word. Reimplemented here because a bound key cannot reach the
# readline original.
_ariadne_backward_kill_word() {
  local left=${READLINE_LINE:0:READLINE_POINT}
  local right=${READLINE_LINE:READLINE_POINT}
  left=${left%%+([[:space:]])}
  left=${left%%+([^[:space:]])}
  READLINE_LINE="${left}${right}"
  READLINE_POINT=${#left}
  _ariadne_after_edit
}

_ariadne_kill_line() {
  READLINE_LINE="${READLINE_LINE:0:READLINE_POINT}"
  _ariadne_after_edit
}

_ariadne_accept_suggestion() {
  if [[ -n $_ARIADNE_GHOST ]] && (( READLINE_POINT == ${#READLINE_LINE} )); then
    READLINE_LINE+=$_ARIADNE_GHOST
    READLINE_POINT=${#READLINE_LINE}
    _ariadne_feedback 0
    _ariadne_clear_state
    _ARIADNE_LAST_BUF=$READLINE_LINE
    _ariadne_set_tab
    _ariadne_paint
  elif (( READLINE_POINT < ${#READLINE_LINE} )); then
    # no ghost: this key is forward-char
    (( READLINE_POINT += 1 ))
  fi
}

_ariadne_select() {
  local -i n=$1
  (( n <= ${#_ARIADNE_CAND[@]} )) || return 0
  READLINE_LINE=${_ARIADNE_CAND[n-1]}
  READLINE_POINT=${#READLINE_LINE}
  _ariadne_feedback $(( n - 1 ))
  _ariadne_clear_state
  _ARIADNE_LAST_BUF=$READLINE_LINE
  _ariadne_set_tab
  _ariadne_paint
}
_ariadne_select_1() { _ariadne_select 1; }
_ariadne_select_2() { _ariadne_select 2; }
_ariadne_select_3() { _ariadne_select 3; }

# C-l: readline's clear-screen is replaced because the real one would leave
# our row bookkeeping stale (the screen is gone but we think rows exist).
_ariadne_clear_screen() {
  _ARIADNE_PAINTED=0 _ARIADNE_RESERVED=0
  printf '\e[H\e[2J'
}

_ariadne_feedback() {
  local chosen=$1 op="ACCEPT"
  (( chosen < 0 )) && op="REJECT"
  _ariadne_send "${op}"$'\t'"sess=$_ARIADNE_SESSION"$'\t'"chosen=$chosen" >/dev/null 2>&1
}

# ---------------------------------------------------------------- ingest

# bash has no preexec. The DEBUG trap fires before every top-level simple
# command — including PROMPT_COMMAND's own entries. The prompt machinery is
# bracketed: precmd (first entry) raises IN_PC, arm (last entry) lowers it
# and arms the trap, so the only fire that can consume ARMED is the user's
# typed command. Note the trap must never return nonzero: a failing DEBUG
# trap would suppress the command it precedes.
_ariadne_dbg() {
  (( _ARIADNE_IN_PC )) && return 0
  # bind -x handler invocations fire this trap as top-level commands, and
  # precmd/arm fire it before IN_PC could be set. Every one of our own
  # functions shares the prefix, so typing and prompt machinery can never
  # consume ARMED — only the user's real command can.
  [[ $BASH_COMMAND == _ariadne_* ]] && return 0
  (( _ARIADNE_ARMED )) || return 0
  _ARIADNE_ARMED=0
  # stored as integer microseconds: EPOCHREALTIME's decimal point is not
  # arithmetic-compatible, and 10# guards octal on zero-padded fractions
  local s=${EPOCHREALTIME%.*} u=${EPOCHREALTIME#*.}
  _ARIADNE_T0=$(( 10#$s * 1000000 + 10#$u ))
  ${_ARIADNE_PREV_DEBUG:+eval "$_ARIADNE_PREV_DEBUG"} 2>/dev/null
  return 0
}

_ariadne_arm() {
  _ARIADNE_IN_PC=0
  _ARIADNE_ARMED=1
  return "${_ARIADNE_PC_CODE:-0}"   # keep $? intact for the prompt render
}

# history 1 is the accepted line as typed — and unlike the keystroke-tracked
# LAST_BUF it also covers bracketed paste, history recall, and yank. LAST_BUF
# is the fallback for `set +o history` users.
_ariadne_history1() {
  local h
  h=$(HISTTIMEFORMAT= history 1 2>/dev/null) || return 1
  [[ $h =~ ^[[:space:]]*[0-9]+[[:space:]]+(.*)$ ]] || return 1
  printf '%s' "${BASH_REMATCH[1]}"
}

_ariadne_precmd() {
  local code=$?
  _ARIADNE_PC_CODE=$code
  _ARIADNE_ARMED=0
  _ARIADNE_IN_PC=1

  # The DEBUG trap is the only "a command actually ran" signal bash has.
  # Without it (empty Enter, Ctrl-C at the prompt) there is nothing to
  # ingest — history 1 would re-report the *previous* command.
  if [[ -n $_ARIADNE_T0 ]]; then
    local cmd
    cmd=$(_ariadne_history1)
    [[ -z $cmd ]] && cmd=$_ARIADNE_LAST_BUF

    # A panel that was shown and typed past is a rejection — negative training
    # data, and the only way the ranker learns what not to surface.
    if (( _ARIADNE_SHOWN )) && [[ -n $cmd ]]; then
      local matched=0 c
      for c in "${_ARIADNE_CAND[@]}"; do
        [[ $c == "$cmd" ]] && { matched=1; break; }
      done
      (( matched )) || _ariadne_feedback -1
    fi

    if [[ -n $cmd ]]; then
      local s=${EPOCHREALTIME%.*} u=${EPOCHREALTIME#*.}
      local -i dur=$(( ( 10#$s * 1000000 + 10#$u - _ARIADNE_T0 ) / 1000 ))
      (( dur < 0 )) && dur=0
      local req="INGEST"
      req+=$'\t'"raw=$(_ariadne_esc "$cmd")"
      req+=$'\t'"cwd=$(_ariadne_esc "$PWD")"
      req+=$'\t'"repo=$(_ariadne_esc "$_ARIADNE_REPO")"
      req+=$'\t'"branch=$(_ariadne_esc "$_ARIADNE_BRANCH")"
      req+=$'\t'"host=$(_ariadne_esc "$_ARIADNE_HOST")"
      req+=$'\t'"sess=$_ARIADNE_SESSION"
      req+=$'\t'"exit=$code"
      req+=$'\t'"dur=$dur"
      req+=$'\t'"ts=$EPOCHSECONDS"
      _ariadne_send "$req" >/dev/null 2>&1
    fi
    _ARIADNE_T0=""
  fi

  # reset per-prompt state
  _ariadne_clear_state
  _ARIADNE_SHOWN=0 _ARIADNE_PAINTED=0 _ARIADNE_RESERVED=0
  _ARIADNE_LAST_BUF=""

  _ariadne_chpwd
  _ariadne_measure_prompt

  if (( _ARIADNE_NOTICE )); then
    _ARIADNE_NOTICE=0
    printf 'ariadne: disabled for %ss (daemon unresponsive)\n' "$ARIADNE_BREAKER_COOLDOWN" >&2
  fi
  return "$code"
}

# PS0 runs in a subshell after accept-line and before execution. It cannot
# mutate state (and must not try); it only repaints. The accepted line is
# redrawn without the trailing ghost, and the reserved panel rows are
# cleared so command output starts on a clean row.
_ariadne_ps0() {
  (( _ARIADNE_PAINTED )) || return 0
  local out="" i cmd
  # cursor is on the first panel row (accept-line emitted CR LF)
  for (( i = 1; i < _ARIADNE_RESERVED; i++ )); do
    out+=$'\r\e[K\e[1B'
  done
  (( _ARIADNE_RESERVED > 0 )) && out+=$'\r\e[K'
  (( _ARIADNE_RESERVED > 0 )) && out+=$'\e['"${_ARIADNE_RESERVED}"$'A\r'

  cmd=$(_ariadne_history1)
  [[ -z $cmd ]] && cmd=$_ARIADNE_LAST_BUF
  # Reprint only when the text is known and terminal-safe; otherwise leave
  # the accepted line alone (a stale dim fragment beats a wiped command).
  # trailing newlines die in command substitution; end on \r so the line
  # break survives $(_ariadne_ps0)
  if [[ -n $cmd && $cmd != *[[:cntrl:]]* ]]; then
    local p=${PS1@P}
    p=${p//[$'\x01'$'\x02']/}
    out+=$'\e[K'"${p}${cmd}"
    out+=$'\n\r'
  elif (( _ARIADNE_RESERVED > 0 )); then
    out+=$'\n\r'
  fi
  printf '%s' "$out"
}

# ---------------------------------------------------------------- wiring

_ariadne_install_insert_bindings() {
  # All printable ASCII. Macro-style rebinding ("a" → "a\C-x…") recurses in
  # readline, so insertion is done by the handler itself.
  local chars=' !"#$%&'"'"'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\]^_`abcdefghijklmnopqrstuvwxyz{|}~'
  local c kspec arg i
  for (( i = 0; i < ${#chars}; i++ )); do
    c=${chars:i:1}
    case $c in
      '"')  kspec='\"' ;;
      '\')  kspec='\\' ;;
      *)    kspec=$c ;;
    esac
    printf -v arg '%q' "$c"
    bind -m emacs -x "\"${kspec}\": _ariadne_insert ${arg}" 2>/dev/null
  done
}

_ariadne_init() {
  # capture the user's Tab binding so ownership loss can restore it verbatim
  _ARIADNE_ORIG_TAB=$(bind -pm emacs 2>/dev/null | grep -F '"\C-i":' | head -n1)

  _ariadne_install_insert_bindings

  bind -m emacs -x '"\C?": _ariadne_backward_delete_char' 2>/dev/null
  bind -m emacs -x '"\C-h": _ariadne_backward_delete_char' 2>/dev/null
  bind -m emacs -x '"\e[3~": _ariadne_delete_char' 2>/dev/null
  bind -m emacs -x '"\C-w": _ariadne_backward_kill_word' 2>/dev/null
  bind -m emacs -x '"\C-k": _ariadne_kill_line' 2>/dev/null
  bind -m emacs -x '"\C-l": _ariadne_clear_screen' 2>/dev/null
  bind -m emacs -x '"\e[C": _ariadne_accept_suggestion' 2>/dev/null  # Right
  bind -m emacs -x '"\eOC": _ariadne_accept_suggestion' 2>/dev/null  # Right (application)
  bind -m emacs -x '"\C-f": _ariadne_accept_suggestion' 2>/dev/null
  bind -m emacs -x '"\e1": _ariadne_select_1' 2>/dev/null            # Alt-1
  bind -m emacs -x '"\e2": _ariadne_select_2' 2>/dev/null
  bind -m emacs -x '"\e3": _ariadne_select_3' 2>/dev/null

  # hooks. precmd must be first (it reads $?) and arm must be last (so the
  # DEBUG trap only fires for the user's command, not the prompt machinery).
  if declare -p PROMPT_COMMAND 2>/dev/null | grep -q '^declare -a'; then
    PROMPT_COMMAND=(_ariadne_precmd "${PROMPT_COMMAND[@]}" _ariadne_arm)
  else
    PROMPT_COMMAND="_ariadne_precmd${PROMPT_COMMAND:+;$PROMPT_COMMAND};_ariadne_arm"
  fi

  # PS0: ours first — user content lands on already-cleaned rows
  PS0='$(_ariadne_ps0)'"${PS0-}"

  # DEBUG trap: chain any existing one
  _ARIADNE_PREV_DEBUG=""
  local prev
  prev=$(trap -p DEBUG)
  if [[ -n $prev ]]; then
    prev=${prev#trap -- \'}
    prev=${prev%\' DEBUG}
    _ARIADNE_PREV_DEBUG=$prev
  fi
  trap '_ariadne_dbg' DEBUG

  prev=$(trap -p EXIT)
  if [[ -n $prev ]]; then
    prev=${prev#trap -- \'}
    prev=${prev%\' EXIT}
    trap "_ariadne_drop; $prev" EXIT
  else
    trap '_ariadne_drop' EXIT
  fi

  _ariadne_chpwd
  _ariadne_measure_prompt
}

typeset -g _ARIADNE_PREV_DEBUG=""
_ariadne_init

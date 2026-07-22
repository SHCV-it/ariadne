# ariadne.zsh — shell integration
#
# This is the real integration point, not kitty. Kitty receives bytes and
# paints cells; it has no model of the line being edited. ZLE does.
#
# Design invariants, in priority order:
#   1. Never block the prompt. Every socket read has a hard timeout and every
#      widget falls through to stock zsh behaviour on any error.
#   2. Never shadow a live completer. Paths, branches, hosts and contexts stay
#      with the shell's own completion system.
#   3. Never spawn a process per keystroke. We talk to the daemon over
#      zsh/net/socket; fork+exec would cost more than the entire budget.

[[ -o interactive ]] || return 0

zmodload zsh/net/socket 2>/dev/null || {
  print -u2 "ariadne: zsh/net/socket unavailable; integration disabled"
  return 0
}
zmodload zsh/datetime 2>/dev/null
autoload -Uz add-zsh-hook

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

typeset -g  _ARIADNE_FD=0
typeset -g  _ARIADNE_SESSION="${RANDOM}${RANDOM}-$$"
typeset -g  _ARIADNE_HOST="${HOST:-$(hostname 2>/dev/null)}"
typeset -g  _ARIADNE_REPO="" _ARIADNE_BRANCH=""
typeset -g  _ARIADNE_FAILS=0 _ARIADNE_DISABLED_UNTIL=0
typeset -g  _ARIADNE_OWNS=0 _ARIADNE_GHOST=""
typeset -ga _ARIADNE_CAND=() _ARIADNE_DISP=() _ARIADNE_DESC=()
typeset -ga _ARIADNE_META=()
typeset -g  _ARIADNE_SHOWN=0 _ARIADNE_CYCLE=0
typeset -g  _ARIADNE_T0=0

# ---------------------------------------------------------------- transport

_ariadne_enabled() {
  (( _ARIADNE_DISABLED_UNTIL > EPOCHSECONDS )) && return 1
  return 0
}

_ariadne_connect() {
  (( _ARIADNE_FD > 0 )) && return 0
  [[ -S $ARIADNE_SOCKET ]] || return 1
  zsocket $ARIADNE_SOCKET 2>/dev/null || return 1
  _ARIADNE_FD=$REPLY
  return 0
}

_ariadne_drop() {
  (( _ARIADNE_FD > 0 )) && exec {_ARIADNE_FD}>&- 2>/dev/null
  _ARIADNE_FD=0
}

# Trip the breaker after repeated timeouts. A completion system that
# intermittently stalls the prompt gets uninstalled within a week; better to
# disable ourselves loudly than to degrade quietly.
_ariadne_fail() {
  _ariadne_drop
  (( _ARIADNE_FAILS++ ))
  if (( _ARIADNE_FAILS >= ARIADNE_BREAKER_TRIP )); then
    _ARIADNE_DISABLED_UNTIL=$(( EPOCHSECONDS + ARIADNE_BREAKER_COOLDOWN ))
    _ARIADNE_FAILS=0
    zle -M "ariadne: disabled for ${ARIADNE_BREAKER_COOLDOWN}s (daemon unresponsive)" 2>/dev/null
  fi
  return 1
}

_ariadne_esc() {
  local s=$1
  s=${s//\\/\\\\}
  s=${s//$'\t'/\\t}
  s=${s//$'\n'/\\n}
  print -r -- "$s"
}

_ariadne_unesc() {
  local s=$1
  s=${s//\\t/$'\t'}
  s=${s//\\n/$'\n'}
  s=${s//\\\\/\\}
  print -r -- "$s"
}

# _ariadne_send <line> — write a request, read the response into globals.
_ariadne_send() {
  _ariadne_enabled || return 1
  _ariadne_connect || return 1

  print -u $_ARIADNE_FD -r -- "$1" 2>/dev/null || return $(_ariadne_fail)

  _ARIADNE_OWNS=0 _ARIADNE_GHOST=""
  _ARIADNE_CAND=() _ARIADNE_DISP=() _ARIADNE_DESC=() _ARIADNE_META=()

  local line
  local -a f
  local -i guard=0
  while true; do
    if ! read -r -u $_ARIADNE_FD -t $ARIADNE_TIMEOUT line 2>/dev/null; then
      return $(_ariadne_fail)
    fi
    [[ $line == "." ]] && break
    (( ++guard > 64 )) && break
    f=("${(@s:	:)line}")
    case $f[1] in
      OWNS)  _ARIADNE_OWNS=$f[2] ;;
      GHOST) _ARIADNE_GHOST=$(_ariadne_unesc "$f[2]") ;;
      CAND)
        _ARIADNE_CAND+=("$(_ariadne_unesc "$f[2]")")
        _ARIADNE_DISP+=("$(_ariadne_unesc "$f[3]")")
        _ARIADNE_DESC+=("$(_ariadne_unesc "$f[4]")")
        _ARIADNE_META+=("$f[5]|$f[6]|$f[7]|$f[8]")
        ;;
      ERR)   return 1 ;;
    esac
  done
  _ARIADNE_FAILS=0
  return 0
}

# ---------------------------------------------------------------- context

# git context is resolved on directory change only. Running `git rev-parse` per
# keystroke would be a fork per character: ~2ms, i.e. the entire budget, spent
# recomputing something that changes on cd and commit.
_ariadne_chpwd() {
  _ARIADNE_REPO=""
  _ARIADNE_BRANCH=""
  local d=$PWD
  while [[ $d != / && -n $d ]]; do
    if [[ -e $d/.git ]]; then
      _ARIADNE_REPO=$d
      if [[ -f $d/.git/HEAD ]]; then
        local head="${$(<$d/.git/HEAD)}"
        _ARIADNE_BRANCH=${head##*/}
      fi
      break
    fi
    d=${d:h}
  done
}

# ---------------------------------------------------------------- query

_ariadne_query() {
  local buf=$BUFFER
  local -i cur=$CURSOR

  if (( ${#buf} < ARIADNE_MIN_CHARS )) && [[ -n $buf ]]; then
    _ARIADNE_CAND=(); return 1
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

_ariadne_clear() {
  POSTDISPLAY=""
  _ARIADNE_SHOWN=0
  _ARIADNE_CYCLE=0
}

_ariadne_render_ghost() {
  POSTDISPLAY=""
  (( ARIADNE_GHOST )) || return
  [[ -z $_ARIADNE_GHOST ]] && return
  (( CURSOR == ${#BUFFER} )) || return
  POSTDISPLAY=$_ARIADNE_GHOST
  region_highlight+=("${#BUFFER} $(( ${#BUFFER} + ${#POSTDISPLAY} )) fg=8")
}

# The panel is rendered with `zle -M`, which places it directly beneath the
# input line.
#
# The brief asked for it above the prompt. That was changed deliberately:
# painting above the prompt means manipulating the scroll region and assuming
# a known prompt height, which breaks on multi-line prompts, on resize reflow,
# inside tmux, and over ssh with a laggy link. `zle -M` is the mechanism zsh
# provides for exactly this, it is redraw-safe, and it costs nothing. The
# information is in the same place relative to the cursor either way.
_ariadne_render_panel() {
  (( ARIADNE_PANEL_LINES > 0 )) || return
  (( ${#_ARIADNE_CAND} > 0 )) || { zle -M ""; return }

  local dim reset bold cyan
  if (( ARIADNE_COLOR )); then
    dim=$'\e[2m'; reset=$'\e[0m'; bold=$'\e[1m'; cyan=$'\e[36m'
  fi

  local msg="" i meta count ctx src
  local -a m
  for (( i = 1; i <= ${#_ARIADNE_CAND} && i <= ARIADNE_PANEL_LINES; i++ )); do
    m=("${(@s:|:)_ARIADNE_META[i]}")
    count=$m[1]; ctx=$m[2]; src=$m[3]
    local marker="  "
    (( i == _ARIADNE_CYCLE )) && marker="${cyan}▸${reset} "
    local line="${marker}${dim}${i}${reset} ${_ARIADNE_DISP[i]}"
    local tail="${dim}"
    [[ $count != 0 ]] && tail+=" ${count}×"
    [[ -n $ctx ]] && tail+=" ${ctx}"
    [[ $src != hist ]] && tail+=" [${src}]"
    tail+="${reset}"
    [[ -n $_ARIADNE_DESC[i] ]] && line+="  ${dim}— ${_ARIADNE_DESC[i]}${reset}"
    msg+="${line}${tail}"$'\n'
  done
  zle -M "${msg%$'\n'}"
  _ARIADNE_SHOWN=1
}

_ariadne_update() {
  if ! _ariadne_query; then
    _ariadne_clear
    zle -M "" 2>/dev/null
    return
  fi
  _ariadne_render_ghost
  _ariadne_render_panel
}

# ---------------------------------------------------------------- widgets

_ariadne_self_insert() {
  zle .self-insert
  _ariadne_update
}

_ariadne_backward_delete_char() {
  zle .backward-delete-char
  _ariadne_update
}

_ariadne_backward_kill_word() {
  zle .backward-kill-word
  _ariadne_update
}

_ariadne_kill_line() {
  zle .kill-line
  _ariadne_clear
  zle -M ""
}

# Tab. The ownership check is the load-bearing line: when the daemon says it
# does not own the current token, we hand control to the shell's real
# completion system, which knows about branches, hosts and files and will
# always beat a learned frecency model on those.
_ariadne_complete() {
  if ! _ariadne_query || (( ! _ARIADNE_OWNS )) || (( ${#_ARIADNE_CAND} == 0 )); then
    _ARIADNE_CYCLE=0
    zle -M ""
    zle .expand-or-complete
    return
  fi
  (( _ARIADNE_CYCLE++ ))
  (( _ARIADNE_CYCLE > ${#_ARIADNE_CAND} )) && _ARIADNE_CYCLE=1
  BUFFER=$_ARIADNE_CAND[$_ARIADNE_CYCLE]
  CURSOR=${#BUFFER}
  POSTDISPLAY=""
  _ariadne_render_panel
}

_ariadne_accept_suggestion() {
  if [[ -n $POSTDISPLAY ]]; then
    BUFFER+=$POSTDISPLAY
    CURSOR=${#BUFFER}
    POSTDISPLAY=""
    _ariadne_feedback 0
    zle -M ""
  else
    zle .forward-char
  fi
}

_ariadne_select() {
  local n=$1
  (( n <= ${#_ARIADNE_CAND} )) || return
  BUFFER=$_ARIADNE_CAND[$n]
  CURSOR=${#BUFFER}
  POSTDISPLAY=""
  _ariadne_feedback $(( n - 1 ))
  zle -M ""
}
_ariadne_select_1() { _ariadne_select 1 }
_ariadne_select_2() { _ariadne_select 2 }
_ariadne_select_3() { _ariadne_select 3 }

_ariadne_feedback() {
  local chosen=$1
  local op="ACCEPT"
  (( chosen < 0 )) && op="REJECT"
  _ariadne_send "${op}"$'\t'"sess=$_ARIADNE_SESSION"$'\t'"chosen=$chosen" >/dev/null 2>&1
}

_ariadne_accept_line() {
  # If a panel was shown and the user typed past it, that is a rejection —
  # negative training data, and the only way the ranker learns what not to
  # surface.
  if (( _ARIADNE_SHOWN )) && [[ -n $BUFFER ]]; then
    local matched=0 c
    for c in $_ARIADNE_CAND; do
      [[ $c == $BUFFER ]] && { matched=1; break }
    done
    (( matched )) || _ariadne_feedback -1
  fi
  _ariadne_clear
  zle -M ""
  zle .accept-line
}

# ---------------------------------------------------------------- ingest

_ariadne_preexec() {
  _ARIADNE_LAST_CMD=$1
  _ARIADNE_T0=$EPOCHREALTIME
}

_ariadne_precmd() {
  local -i code=$?
  [[ -z $_ARIADNE_LAST_CMD ]] && return
  local -i dur=0
  if (( _ARIADNE_T0 )); then
    dur=$(( (EPOCHREALTIME - _ARIADNE_T0) * 1000 ))
  fi
  local req="INGEST"
  req+=$'\t'"raw=$(_ariadne_esc "$_ARIADNE_LAST_CMD")"
  req+=$'\t'"cwd=$(_ariadne_esc "$PWD")"
  req+=$'\t'"repo=$(_ariadne_esc "$_ARIADNE_REPO")"
  req+=$'\t'"branch=$(_ariadne_esc "$_ARIADNE_BRANCH")"
  req+=$'\t'"host=$(_ariadne_esc "$_ARIADNE_HOST")"
  req+=$'\t'"sess=$_ARIADNE_SESSION"
  req+=$'\t'"exit=$code"
  req+=$'\t'"dur=$dur"
  req+=$'\t'"ts=$EPOCHSECONDS"
  _ariadne_send "$req" >/dev/null 2>&1
  _ARIADNE_LAST_CMD=""
  _ARIADNE_T0=0
}

# ---------------------------------------------------------------- wiring

_ariadne_init() {
  zle -N self-insert            _ariadne_self_insert
  zle -N backward-delete-char   _ariadne_backward_delete_char
  zle -N backward-kill-word     _ariadne_backward_kill_word
  zle -N kill-line              _ariadne_kill_line
  zle -N accept-line            _ariadne_accept_line
  zle -N ariadne-complete       _ariadne_complete
  zle -N ariadne-accept         _ariadne_accept_suggestion
  zle -N ariadne-select-1       _ariadne_select_1
  zle -N ariadne-select-2       _ariadne_select_2
  zle -N ariadne-select-3       _ariadne_select_3

  bindkey '^I'    ariadne-complete       # Tab
  bindkey '^[[C'  ariadne-accept         # Right arrow
  bindkey '^F'    ariadne-accept         # Ctrl-F
  bindkey '^[1'   ariadne-select-1       # Alt-1
  bindkey '^[2'   ariadne-select-2
  bindkey '^[3'   ariadne-select-3

  add-zsh-hook preexec _ariadne_preexec
  add-zsh-hook precmd  _ariadne_precmd
  add-zsh-hook chpwd   _ariadne_chpwd
  _ariadne_chpwd
}

_ariadne_init

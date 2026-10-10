@echo off
rem Switch console to UTF-8; otherwise any Chinese left in this file shows as mojibake on GBK terminals.
chcp 65001 >nul 2>&1
rem ARTEX guardian startup script (Windows)
rem
rem Usage:
rem   start.bat                  Run in foreground (Ctrl-C to stop)
rem   start.bat -addr :9000      Extra args are passed through to artex verbatim
rem
rem It does only one thing: run artex.exe, and decide whether to restart it based on the exit code after the process exits.
rem
rem   0      Normal user stop        -> exit the loop
rem   75     Program-requested restart -> rerun immediately (the page's "one-click update" or "rollback" was clicked)
rem   other  Crash                   -> rerun after backoff (1->2->4... up to 60 seconds)
rem
rem Downloading, SHA256 verification, and swapping are not done here -- artex itself completes them all at startup
rem (selfupdate package). The script is kept foolproof; see the notes at the top of start.sh for details.

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] executable %BIN% not found 1>&2
	exit /b 1
)

set "RESTART_CODE=75"
set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] exited normally
	exit /b 0
)

if "!code!"=="%RESTART_CODE%" (
	rem Update/rollback is ready: after rerunning, artex will complete the swap at startup.
	echo [artex] restart requested (applying new version)...
	set /a delay=1
	goto loop
)

echo [artex] exited abnormally ^(code=!code!^), restarting in !delay!s 1>&2
rem timeout fails in a redirected console, so ping is used as a fallback (a delay of N seconds needs N+1 pings).
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop

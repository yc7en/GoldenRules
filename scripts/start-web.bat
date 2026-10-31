@echo off
setlocal
rem Web console. --strictPort is intentional: when the port is taken Vite
rem silently moves to 5174, but the console decides liveness by port 5173, so
rem the dot would stay red while a second instance runs somewhere else.
rem
rem ASCII-only on purpose: this file's stdout is redirected into the service
rem console's log file, which is read as UTF-8.
cd /d "%~dp0.."
set "ROOT=%CD%"
if not defined WEB_PORT set "WEB_PORT=5173"

cd /d "%ROOT%\web"
if not exist "node_modules" (
    echo [web] node_modules missing, running npm install ...
    call npm install
    if errorlevel 1 (
        echo [web] npm install failed
        if /i not "%~1"=="--no-pause" pause
        exit /b 1
    )
)

echo [web] http://localhost:%WEB_PORT%
call npm run dev -- --host 127.0.0.1 --port %WEB_PORT% --strictPort
exit /b %ERRORLEVEL%

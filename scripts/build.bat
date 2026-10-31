@echo off
setlocal
rem Build both backend binaries. Release profile for the engine on purpose:
rem a debug build is an order of magnitude slower, and every later
rem "why is trial slow" question traces back to it.
rem 注意：本文件是 GBK 编码、CRLF 换行。改成 UTF-8 会让中文在 cmd 里按字节错位截断。
cd /d "%~dp0.."
set "ROOT=%CD%"
set "BIN=%ROOT%\.dev\bin"
if not exist "%BIN%" mkdir "%BIN%"

echo == 构建决策服务 gr-server ==
cargo build --release --manifest-path "%ROOT%\backend\Cargo.toml" -p gr-server
if errorlevel 1 goto fail

echo.
echo == 构建管理平台 platformd ==
pushd "%ROOT%\backend\platform"
go build -o "%BIN%\platformd.exe" ./cmd/platformd
set "RC=%ERRORLEVEL%"
popd
if not "%RC%"=="0" goto fail

echo.
echo 构建完成：引擎 %ROOT%\backend\target\release\gr-server.exe
echo          平台 %BIN%\platformd.exe
exit /b 0

:fail
echo.
echo 构建失败，见上方错误输出。
if /i not "%~1"=="--no-pause" pause
exit /b 1

@echo off
setlocal enabledelayedexpansion
rem 按端口停服务。不按进程名杀：同机上可能还有别人跑的 gr-server，
rem 按名字 taskkill 会误伤，按端口只会命中我们自己拉起的那份。
rem 本文件是 GBK 编码、CRLF 换行，勿改 UTF-8。
if not defined ENGINE_PORT set "ENGINE_PORT=18080"
if not defined PLATFORM_PORT set "PLATFORM_PORT=18081"
if not defined WEB_PORT set "WEB_PORT=5173"

for %%P in (%WEB_PORT% %PLATFORM_PORT% %ENGINE_PORT%) do (
    set "HIT="
    for /f "tokens=5" %%I in ('netstat -ano ^| findstr /c:":%%P " ^| findstr /i "LISTENING"') do (
        set "HIT=1"
        taskkill /PID %%I /T /F >nul 2>&1 && echo 已停止端口 %%P PID %%I
    )
    if not defined HIT echo 端口 %%P 无监听
)
echo.
echo 完成
if /i not "%~1"=="--no-pause" pause

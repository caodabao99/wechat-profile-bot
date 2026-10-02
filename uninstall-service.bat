@echo off
setlocal EnableDelayedExpansion
chcp 65001 >nul
title 卸载 WeChat Profile Bot 服务
cd /d "%~dp0"

set "SERVICE_NAME=WeChatProfileBot"

REM 服务不存在则直接结束
sc query !SERVICE_NAME! >nul 2>&1
if errorlevel 1 (
    echo [提示] 未找到服务 !SERVICE_NAME!，无需卸载。
    echo.
    pause
    exit /b 0
)

if not exist "nssm.exe" goto fallback_sc

echo 正在停止服务...
nssm.exe stop !SERVICE_NAME! 2>nul
echo 正在卸载服务...
nssm.exe remove !SERVICE_NAME! confirm
sc query !SERVICE_NAME! >nul 2>&1
if errorlevel 1 goto done

REM nssm 卸载失败时同样回退到 sc
:fallback_sc
echo [提示] 使用 Windows 自带的 sc 命令卸载（兜底路径）。
echo [提示] 若提示“拒绝访问”，请以管理员身份重新运行本脚本。
echo.
echo 正在停止服务...
sc stop !SERVICE_NAME! >nul 2>&1
echo 正在卸载服务...
sc delete !SERVICE_NAME!
if errorlevel 1 (
    echo [错误] 卸载失败，请以管理员身份重新运行本脚本。
    echo.
    pause
    exit /b 1
)

:done
echo 服务已卸载
echo.
endlocal
pause

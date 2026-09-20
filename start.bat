@echo off
chcp 65001 >nul
title SenseNova Proxy
echo ==============================================
echo        SenseNova Multi-Egress Proxy
echo ==============================================
echo.
if not exist "config.json" (
    echo [ERROR] config.json not found!
    echo Please copy config.example.json to config.json and fill in your keys.
    echo.
    pause
    exit /b 1
)

echo Starting proxy using config.json...
sensenova-proxy.exe -c config.json
pause

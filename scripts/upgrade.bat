@echo off
cd /d "%~dp0"
"%~dp0simpleHtmlWatch.exe" -adopt-build %*
if errorlevel 1 pause

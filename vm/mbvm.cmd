@echo off
rem mbvm from PowerShell or cmd: runs vm/mbvm (a bash script) with Git for
rem Windows' bash. Same commands and settings: vm\mbvm help
setlocal
set "MBVM_BASH="
for %%B in ("%ProgramFiles%\Git\bin\bash.exe" "%ProgramFiles(x86)%\Git\bin\bash.exe" "%LocalAppData%\Programs\Git\bin\bash.exe") do if not defined MBVM_BASH if exist %%B set "MBVM_BASH=%%~B"
if not defined MBVM_BASH (
  echo error: Git for Windows is not installed ^(it brings the bash mbvm runs in^): winget install Git.Git 1>&2
  exit /b 1
)
"%MBVM_BASH%" "%~dp0mbvm" %*

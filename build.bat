@echo off
setlocal

REM Build script for CoderJia TimeKeeper
set EXE_NAME=timekeeper.exe
set APP_NAME=CoderJia-TimeKeeper

echo [1/2] Building %EXE_NAME% ...
go build -o %EXE_NAME% .
if errorlevel 1 (
    echo Build failed.
    exit /b 1
)

echo [2/2] Build success: %EXE_NAME%
echo.
echo Common commands:
echo   %EXE_NAME% -install
echo   %EXE_NAME% -remove
echo   sc query %APP_NAME%

endlocal

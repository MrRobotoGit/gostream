@echo off
setlocal EnableExtensions EnableDelayedExpansion

REM GoStream Windows stack generator (Dockge-friendly)
REM - Generates an auto-contained stack folder with compose.yaml, .env, Dockerfile, custom-* and src\ build context
REM - Optionally deploys via docker compose (default)
REM - Never deletes user data directories

set "SCRIPT_DIR=%~dp0"
for %%I in ("%SCRIPT_DIR%..") do set "REPO_ROOT=%%~fI"
set "TEMPLATES_DIR=%SCRIPT_DIR%templates"

set "DEFAULT_BASE=%USERPROFILE%\Documents\Docker Stuff"
set "DEFAULT_STACKS=%USERPROFILE%\Documents\Docker Stuff\Dockge\stacks"

set "FLAVOR="
set "MODE="
set "BASE_DIR_WIN="
set "STACKS_ROOT_WIN="
set "NON_INTERACTIVE=0"
set "NO_DEPLOY=0"
set "USE_EXISTING_FILES=0"
set "EXISTING_COMPOSE_FILE="
set "EXISTING_DOCKERFILE="
set "MYDEPLOY_PREASK=0"
set "USE_MYDEPLOY=0"

set "PUID=1000"
set "PGID=1000"
set "TZ=America/La_Paz"
set "PLEX_CLAIM="

set "PLEX_IMPORT=0"
set "PLEX_IMPORT_SRC="

set "CONFIG_CREATED=0"
set "CONFIG_CHANGED=0"
set "CONFIG_BACKUP="
set "GOSTREAM_METRICS_PORT="

call :main %*
set "RC=%ERRORLEVEL%"

echo.
if "%RC%"=="0" (
  echo install-rebuild finished successfully.
) else (
  echo install-rebuild failed with exit code %RC%.
)
if /i "%SKIP_PAUSE%"=="1" exit /b %RC%
echo Press any key to close...
pause >nul
exit /b %RC%

REM -------------------------
REM Arg parsing
REM -------------------------
:main
:parse_args
if "%~1"=="" goto args_done

if /i "%~1"=="--help" goto help
if /i "%~1"=="-h" goto help

if /i "%~1"=="--non-interactive" (
  set "NON_INTERACTIVE=1"
  shift
  goto parse_args
)

if /i "%~1"=="--no-deploy" (
  set "NO_DEPLOY=1"
  shift
  goto parse_args
)

if /i "%~1"=="--use-existing-files" (
  set "USE_EXISTING_FILES=1"
  shift
  goto parse_args
)

if /i "%~1"=="--flavor" (
  if "%~2"=="" goto bad_args
  set "FLAVOR=%~2"
  shift
  shift
  goto parse_args
)

if /i "%~1"=="--mode" (
  if "%~2"=="" goto bad_args
  set "MODE=%~2"
  shift
  shift
  goto parse_args
)

if /i "%~1"=="--base" (
  if "%~2"=="" goto bad_args
  set "BASE_DIR_WIN=%~2"
  shift
  shift
  goto parse_args
)

if /i "%~1"=="--stacks" (
  if "%~2"=="" goto bad_args
  set "STACKS_ROOT_WIN=%~2"
  shift
  shift
  goto parse_args
)

if /i "%~1"=="--compose-file" (
  if "%~2"=="" goto bad_args
  set "EXISTING_COMPOSE_FILE=%~2"
  shift
  shift
  goto parse_args
)

if /i "%~1"=="--dockerfile-file" (
  if "%~2"=="" goto bad_args
  set "EXISTING_DOCKERFILE=%~2"
  shift
  shift
  goto parse_args
)

echo ERROR: Unknown argument: %~1
goto bad_args

:args_done

REM -------------------------
REM Validate / interactive prompts
REM -------------------------

if /i "%FLAVOR%"=="plex" set "FLAVOR=plex"
if /i "%FLAVOR%"=="jellyfin" set "FLAVOR=jellyfin"

if "%MODE%"=="" (
  if "%NON_INTERACTIVE%"=="1" (
    echo ERROR: --mode is required in --non-interactive mode.
    goto bad_args
  )
  set "MODE=A"
)

if /i not "%MODE%"=="A" (
  echo ERROR: Only --mode A is supported.
  goto bad_args
)

if "%NON_INTERACTIVE%"=="1" (
  if "%FLAVOR%"=="" (
    echo ERROR: --flavor plex^|jellyfin is required in --non-interactive mode.
    goto bad_args
  )
  if "%BASE_DIR_WIN%"=="" (
    echo ERROR: --base "..." is required in --non-interactive mode.
    goto bad_args
  )
  if "%STACKS_ROOT_WIN%"=="" (
    echo ERROR: --stacks "..." is required in --non-interactive mode.
    goto bad_args
  )
  if "%USE_EXISTING_FILES%"=="1" (
    if "%EXISTING_COMPOSE_FILE%"=="" (
      echo ERROR: --compose-file is required when --use-existing-files is set.
      goto bad_args
    )
    if "%EXISTING_DOCKERFILE%"=="" (
      echo ERROR: --dockerfile-file is required when --use-existing-files is set.
      goto bad_args
    )
  )
) else (
  call :prompt_mydeploy_preflavor
  if errorlevel 1 exit /b !ERRORLEVEL!

  if "!FLAVOR!"=="" call :prompt_flavor
  if not "!USE_MYDEPLOY!"=="1" (
    if "!BASE_DIR_WIN!"=="" call :prompt_value "Base path" "%DEFAULT_BASE%" BASE_DIR_WIN
    if "!STACKS_ROOT_WIN!"=="" call :prompt_value "Stacks root path" "%DEFAULT_STACKS%" STACKS_ROOT_WIN

    call :prompt_source_mode
    if errorlevel 1 exit /b !ERRORLEVEL!

    call :prompt_runtime_options
    if errorlevel 1 exit /b !ERRORLEVEL!

    if /i "!FLAVOR!"=="plex" (
      set /p "PLEX_CLAIM=Plex claim token (optional; blank to skip): "
      call :prompt_plex_import
      if errorlevel 1 exit /b !ERRORLEVEL!
    )
  )
)

 if /i not "%FLAVOR%"=="plex" if /i not "%FLAVOR%"=="jellyfin" (
   echo ERROR: Invalid flavor: %FLAVOR%
   goto bad_args
 )

 REM -------------------------
 REM Preflight: stop Plex (Windows + container) before any deploy/generation
 REM - Must run even when --no-deploy is specified
 REM -------------------------
 call :preflight_stop_plex
 if errorlevel 1 exit /b !ERRORLEVEL!

 if "%USE_MYDEPLOY%"=="1" (
   REM my-deploy stack root: use --stacks if provided, else DEFAULT_STACKS (no prompting)
   set "MYDEPLOY_STACKS_ROOT=%STACKS_ROOT_WIN%"
   if "!MYDEPLOY_STACKS_ROOT!"=="" set "MYDEPLOY_STACKS_ROOT=%DEFAULT_STACKS%"
   call :fullpath "!MYDEPLOY_STACKS_ROOT!" MYDEPLOY_STACKS_ROOT
   set "MYDEPLOY_STACK_DIR=!MYDEPLOY_STACKS_ROOT!\gostream-plex"
   call :materialize_mydeploy_stack "!EXISTING_COMPOSE_FILE!" "!MYDEPLOY_STACK_DIR!" "!REPO_ROOT!" MYDEPLOY_COMPOSE_FILE
   if errorlevel 1 exit /b !ERRORLEVEL!

  echo.
  echo my-deploy mode selected.
  echo   Source compose : !EXISTING_COMPOSE_FILE!
  echo   Dockerfile file: !EXISTING_DOCKERFILE!
  echo   Dockge stack   : !MYDEPLOY_STACK_DIR!
  echo   Compose file   : !MYDEPLOY_COMPOSE_FILE!
   if "%NO_DEPLOY%"=="1" (
     echo NOTE: --no-deploy specified; skipping docker compose up.
     exit /b 0
   )
   call :deploy_existing_compose "!MYDEPLOY_COMPOSE_FILE!"
   exit /b !ERRORLEVEL!
 )

if not exist "%TEMPLATES_DIR%\compose.yaml.tmpl" (
  echo ERROR: Missing templates. Expected: %TEMPLATES_DIR%\compose.yaml.tmpl
  exit /b 1
)

if not exist "%REPO_ROOT%\config.json.example" (
  echo ERROR: Missing %REPO_ROOT%\config.json.example
  exit /b 1
)

REM Normalize base/stacks root to full Windows paths
call :fullpath "%BASE_DIR_WIN%" BASE_DIR_WIN
call :fullpath "%STACKS_ROOT_WIN%" STACKS_ROOT_WIN

if "%USE_EXISTING_FILES%"=="1" (
  call :fullpath "%EXISTING_COMPOSE_FILE%" EXISTING_COMPOSE_FILE
  call :fullpath "%EXISTING_DOCKERFILE%" EXISTING_DOCKERFILE
  if not exist "%EXISTING_COMPOSE_FILE%" (
    echo ERROR: Existing compose file not found: %EXISTING_COMPOSE_FILE%
    exit /b 1
  )
  if not exist "%EXISTING_DOCKERFILE%" (
    echo ERROR: Existing Dockerfile not found: %EXISTING_DOCKERFILE%
    exit /b 1
  )
)

REM Compute container name + stack dir
if /i "%FLAVOR%"=="plex" (
  set "CONTAINER_NAME=gostream-plex"
) else (
  set "CONTAINER_NAME=gostream-jellyfin"
)
set "STACK_DIR_WIN=%STACKS_ROOT_WIN%\%CONTAINER_NAME%"

echo.
echo Target paths:
echo   Base data   : %BASE_DIR_WIN%
echo   Stacks root : %STACKS_ROOT_WIN%
echo   Stack dir   : %STACK_DIR_WIN%

REM Create required host directories (idempotent; never delete)
call :ensure_dir "%BASE_DIR_WIN%"
call :ensure_dir "%STACKS_ROOT_WIN%"
call :ensure_dir "%STACK_DIR_WIN%"

call :ensure_dir "%BASE_DIR_WIN%\gostream"
call :ensure_dir "%BASE_DIR_WIN%\gostream-mkv-real\backup"
call :ensure_dir "%BASE_DIR_WIN%\gostream-mkv-real\config"
call :ensure_dir "%BASE_DIR_WIN%\gostream-mkv-real\movies"
call :ensure_dir "%BASE_DIR_WIN%\gostream-mkv-real\tv"
call :ensure_dir "%BASE_DIR_WIN%\%CONTAINER_NAME%\config"
call :ensure_dir "%BASE_DIR_WIN%\%CONTAINER_NAME%\transcode"

REM Ensure config.json exists
set "CONFIG_PATH=%BASE_DIR_WIN%\gostream-mkv-real\config\config.json"
if not exist "%CONFIG_PATH%" (
  copy /y "%REPO_ROOT%\config.json.example" "%CONFIG_PATH%" >nul
  if errorlevel 1 (
    echo ERROR: Failed to create config.json from config.json.example
    exit /b 1
  )
  set "CONFIG_CREATED=1"
)

 REM Patch config.json + read internal metrics port
 call :patch_config "%CONFIG_PATH%" "%BASE_DIR_WIN%\gostream-mkv-real\backup" "%FLAVOR%"
 if errorlevel 1 (
   echo ERROR: Failed to patch/read config.json at %CONFIG_PATH%
   exit /b !ERRORLEVEL!
 )
 if "%GOSTREAM_METRICS_PORT%"=="" (
   echo ERROR: Failed to determine internal metrics port from config.json
   exit /b 1
 )

 REM Fixed host ports (no auto-selection)
 set "MEDIA_HOST_PORT=32302"
 set "GOSTORM_HOST_PORT=8090"
 set "HEALTH_HOST_PORT=8095"
 set "METRICS_HOST_PORT=8096"
 if /i "%FLAVOR%"=="plex" (
   set "MEDIA_CONTAINER_PORT=32400"
 ) else (
   set "MEDIA_CONTAINER_PORT=8096"
 )

REM Normalize BASE_DIR and STACK_DIR for .env (forward slashes)
call :norm_fwd "%BASE_DIR_WIN%" BASE_DIR_FWD
if errorlevel 1 (
  echo ERROR: Failed to normalize base path for .env.
  exit /b !ERRORLEVEL!
)
call :norm_fwd "%STACK_DIR_WIN%" STACK_DIR_FWD
if errorlevel 1 (
  echo ERROR: Failed to normalize stack path for .env.
  exit /b !ERRORLEVEL!
)

REM Generate stack files
call :generate_stack "%FLAVOR%" "%STACK_DIR_WIN%" "%BASE_DIR_FWD%" "%STACK_DIR_FWD%" "%CONTAINER_NAME%" "%USE_EXISTING_FILES%" "%EXISTING_COMPOSE_FILE%" "%EXISTING_DOCKERFILE%"
if errorlevel 1 (
  echo ERROR: Failed while generating stack files in %STACK_DIR_WIN%
  exit /b !ERRORLEVEL!
)

REM Optional Plex import (interactive only)
if /i "%FLAVOR%"=="plex" (
  if "%NON_INTERACTIVE%"=="1" (
    REM Safety: no import in non-interactive mode
    set "PLEX_IMPORT=0"
  )
  if "%PLEX_IMPORT%"=="1" (
    call :do_plex_import "%PLEX_IMPORT_SRC%" "%BASE_DIR_WIN%\gostream-plex\config\Library\Application Support\Plex Media Server"
    if errorlevel 1 exit /b !ERRORLEVEL!
  )
)

REM Deploy (default)
if "%NO_DEPLOY%"=="1" (
  echo.
  echo NOTE: --no-deploy specified; skipping docker compose up.
) else (
  call :deploy_stack "%STACK_DIR_WIN%"
  if errorlevel 1 exit /b !ERRORLEVEL!
)

REM Summary
echo.
echo ===================== SUMMARY =====================
echo Flavor              : %FLAVOR%
echo Mode                : %MODE%
echo Base (Windows)      : %BASE_DIR_WIN%
echo Stacks root (Win)   : %STACKS_ROOT_WIN%
echo Stack dir (Win)     : %STACK_DIR_WIN%
echo Container name      : %CONTAINER_NAME%
if "%USE_EXISTING_FILES%"=="1" (
  echo Source mode        : existing user files
  echo Compose source     : %EXISTING_COMPOSE_FILE%
  echo Dockerfile source  : %EXISTING_DOCKERFILE%
) else (
  echo Source mode        : built-in templates ^(docker-windows/templates^)
)
echo.
 echo Ports (host -> container):
 echo   MEDIA_SERVER       : %MEDIA_HOST_PORT% ^> %MEDIA_CONTAINER_PORT%
 echo   GOSTORM_API        : %GOSTORM_HOST_PORT% ^> 8090
 echo   HEALTH_MONITOR     : %HEALTH_HOST_PORT% ^> 8095
 echo   GOSTREAM_METRICS   : %METRICS_HOST_PORT% ^> %GOSTREAM_METRICS_PORT%
  echo   Internal metrics   : %GOSTREAM_METRICS_PORT%
echo.
echo config.json:
if "%CONFIG_CREATED%"=="1" (echo   - created from config.json.example) else (echo   - existed)
if "%CONFIG_CHANGED%"=="1" (echo   - patched keys) else (echo   - no changes required)
if /i "%FLAVOR%"=="jellyfin" (
  if not "%CONFIG_BACKUP%"=="" echo   - backup: %CONFIG_BACKUP%
)
echo.
if /i "%FLAVOR%"=="plex" (
  if "%PLEX_IMPORT%"=="1" (
    echo Plex import        : YES
    echo Plex import source : %PLEX_IMPORT_SRC%
  ) else (
    echo Plex import        : NO
  )
)
echo ===================================================

exit /b 0


REM =====================================================
REM Helpers
REM =====================================================

:help
echo.
echo Usage:
echo   install-rebuild.bat [--help] [--no-deploy] [--use-existing-files --compose-file "..." --dockerfile-file "..."]
echo.
echo Interactive (prompts):
echo   install-rebuild.bat
echo.
echo Non-interactive:
echo   install-rebuild.bat --flavor plex^|jellyfin --mode A --base "..." --stacks "..." --non-interactive [--no-deploy] [--use-existing-files --compose-file "..." --dockerfile-file "..."]
echo.
echo Flags:
echo   --flavor plex^|jellyfin   Select media server flavor
echo   --mode A                 Mode A only (recreate container; never wipe user data)
echo   --base "PATH"            Base data directory (host)
echo   --stacks "PATH"          Dockge stacks root (host)
echo   --non-interactive        Fail instead of prompting
echo   --no-deploy              Generate stack files but do not run docker compose up
echo   --use-existing-files     Use user-provided compose/Dockerfile instead of built-in templates
echo   --compose-file "PATH"    Existing compose file to copy into stack as compose.yaml
echo   --dockerfile-file "PATH" Existing Dockerfile to copy into stack as Dockerfile
echo   --help                   Show this help
echo.
exit /b 0

:bad_args
echo.
echo Run with --help for usage.
exit /b 2

REM -------------------------
REM Preflight helpers
REM -------------------------

:preflight_stop_plex
REM Stop Docker containers (if they exist)
call :stop_docker_container_if_exists "gostream-plex"
call :stop_docker_container_if_exists "gostream-jellyfin"

REM Stop Windows Plex Media Server process
call :stop_windows_plex_media_server
if errorlevel 1 exit /b !ERRORLEVEL!

REM Verify fixed ports are now free (abort; do not auto-change)
call :assert_fixed_ports_free
if errorlevel 1 exit /b !ERRORLEVEL!

exit /b 0

:assert_fixed_ports_free
set "_portsFailed=0"
call :assert_port_free 32302
if errorlevel 1 set "_portsFailed=1"
call :assert_port_free 8090
if errorlevel 1 set "_portsFailed=1"
call :assert_port_free 8095
if errorlevel 1 set "_portsFailed=1"
call :assert_port_free 8096
if errorlevel 1 set "_portsFailed=1"

if "%_portsFailed%"=="1" (
  echo.
  echo ERROR: One or more required fixed ports are in use. Aborting.
  exit /b 3
)

exit /b 0

:assert_port_free
set "_port=%~1"
if "%_port%"=="" exit /b 1

set "__CHECK_PORT=%_port%"
set "_found=0"
 for /f "usebackq delims=" %%L in (`powershell -NoProfile -ExecutionPolicy Bypass -Command "& { $port=[int]$env:__CHECK_PORT; $owningPids=@(); $haveCmd = (Get-Command Get-NetTCPConnection -ErrorAction SilentlyContinue) -ne $null; if($haveCmd){ try { $owningPids = @(Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue | Select-Object -ExpandProperty OwningProcess -Unique) } catch { $owningPids=@() } } else { $owningPids=@(); try { netstat -ano -p TCP | ForEach-Object { $line=$_.Trim(); if($line -match '^TCP\\s+([^\\s]+)\\s+([^\\s]+)\\s+LISTENING\\s+(\\d+)$'){ $local=$Matches[1]; $opidNum=[int]$Matches[3]; if($local -match (':'+$port+'$') -or $local -match ('\\]:'+ $port +'$')){ $owningPids += $opidNum } } } } catch { $owningPids=@() }; $owningPids = @($owningPids | Sort-Object -Unique) }; if(-not $owningPids -or $owningPids.Count -eq 0){ exit 0 }; foreach($opidNum in $owningPids){ $procObj = Get-Process -Id $opidNum -ErrorAction SilentlyContinue; $name = if($procObj){$procObj.ProcessName}else{'<unknown>'}; $path = '<unknown>'; try { if($procObj -and $procObj.Path){ $path=$procObj.Path } } catch { $path='<access denied>' }; Write-Output ('INUSE|'+$port+'|'+$opidNum+'|'+$name+'|'+$path) }; exit 0 }"`) do (
  set "_found=1"
  for /f "tokens=2-5 delims=|" %%P in ("%%L") do (
    echo Port %%P is in use ^> PID %%Q ^> %%R ^> %%S
  )
)
set "__CHECK_PORT="

if "%_found%"=="1" exit /b 1
exit /b 0

:stop_docker_container_if_exists
set "_name=%~1"
if "%_name%"=="" exit /b 0

where docker >nul 2>&1
if errorlevel 1 (
  REM Allow generation without docker installed
  exit /b 0
)

docker container inspect "%_name%" >nul 2>&1
if errorlevel 1 exit /b 0

echo.
echo [preflight] Stopping Docker container "%_name%" (if running)...
docker stop -t 10 "%_name%" >nul 2>&1

exit /b 0

:is_admin
net session >nul 2>&1
if errorlevel 1 (
  exit /b 1
)
exit /b 0

:stop_windows_plex_media_server
REM Try non-elevated graceful stop first, then force kill.
call :_stop_windows_plex_media_server_non_elevated
set "_rc=%ERRORLEVEL%"
if "%_rc%"=="0" exit /b 0

if "%_rc%"=="5" (
  REM Access denied - request elevation and retry.
  call :is_admin
  if not errorlevel 1 (
    echo.
    echo ERROR: Plex Media Server process could not be stopped even as admin.
    exit /b 1
  )

  echo.
  echo [preflight] Plex Media Server requires admin privileges to stop.
  echo [preflight] A UAC prompt may appear; approve to continue.
  call :_stop_windows_plex_media_server_elevated
  if errorlevel 1 (
    echo.
    echo ERROR: Elevated Plex stop failed (UAC cancelled or access denied).
    exit /b 1
  )

  REM Re-check (non-elevated) to confirm it is gone.
  call :_stop_windows_plex_media_server_non_elevated
  if errorlevel 1 (
    echo.
    echo ERROR: Plex Media Server is still running after elevated stop attempt.
    exit /b 1
  )
  exit /b 0
)

echo.
echo ERROR: Failed to stop Windows Plex Media Server process.
exit /b 1

:_stop_windows_plex_media_server_non_elevated
set "__PLEX_PS_NAME=Plex Media Server"
powershell -NoProfile -ExecutionPolicy Bypass -Command "& { $n=$env:__PLEX_PS_NAME; $procs=@(Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.ProcessName -ieq $n }); if($procs.Count -eq 0){ exit 0 }; foreach($p in $procs){ try { [void]$p.CloseMainWindow() } catch {} }; Start-Sleep -Seconds 10; $procs=@(Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.ProcessName -ieq $n }); if($procs.Count -eq 0){ exit 0 }; try { $procs | Stop-Process -Force -ErrorAction Stop } catch { if($_.Exception -is [System.ComponentModel.Win32Exception] -and $_.Exception.NativeErrorCode -eq 5){ exit 5 }; exit 1 }; Start-Sleep -Milliseconds 250; $procs=@(Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.ProcessName -ieq $n }); if($procs.Count -eq 0){ exit 0 }; exit 2 }" >nul
set "_rc=%ERRORLEVEL%"
set "__PLEX_PS_NAME="
exit /b %_rc%

:_stop_windows_plex_media_server_elevated
set "__PLEX_PS_NAME=Plex Media Server"
set "__PLEX_STOP_CMD=& { $n=$env:__PLEX_PS_NAME; $procs=@(Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.ProcessName -ieq $n }); if($procs.Count -eq 0){ exit 0 }; foreach($p in $procs){ try { [void]$p.CloseMainWindow() } catch {} }; Start-Sleep -Seconds 10; $procs=@(Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.ProcessName -ieq $n }); if($procs.Count -eq 0){ exit 0 }; $procs | Stop-Process -Force -ErrorAction Stop; exit 0 }"

powershell -NoProfile -ExecutionPolicy Bypass -Command "& { $cmd=$env:__PLEX_STOP_CMD; try { $p = Start-Process -FilePath 'powershell' -Verb RunAs -Wait -PassThru -ArgumentList @('-NoProfile','-ExecutionPolicy','Bypass','-Command', $cmd); exit $p.ExitCode } catch { exit 1 } }" >nul

set "__PLEX_PS_NAME="
set "__PLEX_STOP_CMD="
exit /b %ERRORLEVEL%


:prompt_value
set "_label=%~1"
set "_def=%~2"
set "_outvar=%~3"
set "_in="
set /p "_in=%_label% [%_def%]: "
if "%_in%"=="" set "_in=%_def%"
set "%_outvar%=%_in%"
exit /b 0

:prompt_flavor
echo.
echo Select flavor:
echo   1) Plex
echo   2) Jellyfin
:prompt_flavor_loop
set "_ans="
set /p "_ans=Enter 1 or 2: "
if "%_ans%"=="1" (set "FLAVOR=plex" & exit /b 0)
if "%_ans%"=="2" (set "FLAVOR=jellyfin" & exit /b 0)
echo Invalid selection.
goto prompt_flavor_loop

:prompt_mydeploy_preflavor
set "_myCompose=%SCRIPT_DIR%my-deploy\my-deploy.compose.yaml"
set "_myDockerfile=%SCRIPT_DIR%my-deploy\my-deploy.Dockerfile"
if not exist "%_myCompose%" exit /b 0
if not exist "%_myDockerfile%" exit /b 0

if "%USE_EXISTING_FILES%"=="1" exit /b 0

echo.
echo Found my-deploy files:
echo   %_myCompose%
echo   %_myDockerfile%
set "MYDEPLOY_PREASK=1"
set "_usemy="
set /p "_usemy=Use my-deploy files? [Y/N] (default N): "
if "%_usemy%"=="" set "_usemy=N"
if /i "%_usemy%"=="Y" (
  set "USE_MYDEPLOY=1"
  set "USE_EXISTING_FILES=1"
  set "EXISTING_COMPOSE_FILE=%_myCompose%"
  set "EXISTING_DOCKERFILE=%_myDockerfile%"
  set "FLAVOR=plex"
  echo Using my-deploy files.
  exit /b 0
)
if /i "%_usemy%"=="N" exit /b 0
echo Invalid choice; expected Y or N.
goto prompt_mydeploy_preflavor

:prompt_source_mode
if "%USE_EXISTING_FILES%"=="1" if not "%EXISTING_COMPOSE_FILE%"=="" if not "%EXISTING_DOCKERFILE%"=="" exit /b 0

echo.
set "_myCompose=%SCRIPT_DIR%my-deploy\my-deploy.compose.yaml"
set "_myDockerfile=%SCRIPT_DIR%my-deploy\my-deploy.Dockerfile"
if not "%MYDEPLOY_PREASK%"=="1" if exist "%_myCompose%" if exist "%_myDockerfile%" (
  echo Found my-deploy files:
  echo   %_myCompose%
  echo   %_myDockerfile%
  set "_usemy="
  set /p "_usemy=Use my-deploy files? [Y/N] (default N): "
  if "%_usemy%"=="" set "_usemy=N"
  if /i "%_usemy%"=="Y" (
    set "USE_EXISTING_FILES=1"
    set "EXISTING_COMPOSE_FILE=%_myCompose%"
    set "EXISTING_DOCKERFILE=%_myDockerfile%"
    exit /b 0
  )
)

echo Installation source:
echo   1^) Start from scratch ^(use docker-windows/templates^)
echo   2^) Use existing files you already created
:prompt_source_mode_loop
set "_srcans="
set /p "_srcans=Enter 1 or 2 [1]: "
if "%_srcans%"=="" set "_srcans=1"
if "%_srcans%"=="1" (
  set "USE_EXISTING_FILES=0"
  set "EXISTING_COMPOSE_FILE="
  set "EXISTING_DOCKERFILE="
  exit /b 0
)
if "%_srcans%"=="2" (
  set "USE_EXISTING_FILES=1"
  if /i "%FLAVOR%"=="plex" (
    set "_defaultName=gostream-plex"
  ) else (
    set "_defaultName=gostream-jellyfin"
  )
  set "_defaultCompose=%STACKS_ROOT_WIN%\%_defaultName%\compose.yaml"
  set "_defaultDockerfile=%STACKS_ROOT_WIN%\%_defaultName%\Dockerfile"
  call :prompt_value "Existing compose file path" "%_defaultCompose%" EXISTING_COMPOSE_FILE
  call :prompt_value "Existing Dockerfile path" "%_defaultDockerfile%" EXISTING_DOCKERFILE
  if not exist "%EXISTING_COMPOSE_FILE%" (
    echo ERROR: File not found: %EXISTING_COMPOSE_FILE%
    exit /b 1
  )
  if not exist "%EXISTING_DOCKERFILE%" (
    echo ERROR: File not found: %EXISTING_DOCKERFILE%
    exit /b 1
  )
  exit /b 0
)
echo Invalid selection.
goto prompt_source_mode_loop

:prompt_runtime_options
echo.
set "_runopt="
set /p "_runopt=Use recommended runtime defaults (PUID=1000, PGID=1000, TZ=America/La_Paz)? [Y/N] (default Y): "
if "%_runopt%"=="" set "_runopt=Y"
if /i "%_runopt%"=="Y" exit /b 0
if /i "%_runopt%"=="N" (
  call :prompt_value "PUID" "%PUID%" PUID
  call :prompt_value "PGID" "%PGID%" PGID
  call :prompt_value "TZ" "%TZ%" TZ
  exit /b 0
)
echo Invalid choice; expected Y or N.
goto prompt_runtime_options

:prompt_plex_import
set "_default=N"
if exist "%LOCALAPPDATA%\Plex Media Server" set "_default=Y"
echo.
set "_ans="
set /p "_ans=Import existing Plex data from Windows? [Y/N] (default %_default%): "
if "%_ans%"=="" set "_ans=%_default%"
if /i "%_ans%"=="Y" (
  set "PLEX_IMPORT=1"
  call :prompt_value "Plex source path" "%LOCALAPPDATA%\Plex Media Server" PLEX_IMPORT_SRC
  if not exist "%PLEX_IMPORT_SRC%" (
    echo ERROR: Plex source path does not exist: %PLEX_IMPORT_SRC%
    exit /b 1
  )
  exit /b 0
)
if /i "%_ans%"=="N" (
  set "PLEX_IMPORT=0"
  exit /b 0
)
echo Invalid choice; expected Y or N.
goto prompt_plex_import

:fullpath
set "_in=%~1"
set "_out=%~2"
for %%I in ("%_in%") do set "%_out%=%%~fI"
exit /b 0

:norm_fwd
set "_in=%~1"
set "_out=%~2"
for %%I in ("%_in%") do set "__PS_IN=%%~fI"
for /f "usebackq delims=" %%I in (`powershell -NoProfile -ExecutionPolicy Bypass -Command "$p=$env:__PS_IN; $p=$p.TrimEnd('\\'); ($p -replace '\\','/')"`) do set "%_out%=%%I"
set "__PS_IN="
exit /b 0

:patch_config
set "_cfg=%~1"
set "_backupDir=%~2"
set "_flavor=%~3"

set "__CFG=%_cfg%"
set "__BAK=%_backupDir%"
set "__FLAVOR=%_flavor%"

 for /f "usebackq tokens=1* delims==" %%A in (`powershell -NoProfile -ExecutionPolicy Bypass -Command "& { $cfg=$env:__CFG; $backupDir=$env:__BAK; $flavor=$env:__FLAVOR; if(-not (Test-Path -LiteralPath $cfg)) { throw 'config.json missing' }; $raw=Get-Content -Raw -LiteralPath $cfg; $obj=$raw | ConvertFrom-Json; $changed=$false; if($null -eq $obj.physical_source_path -or $obj.physical_source_path -ne '/gostream/source'){ $obj.physical_source_path='/gostream/source'; $changed=$true }; if($null -eq $obj.fuse_mount_path -or $obj.fuse_mount_path -ne '/gostream/mount'){ $obj.fuse_mount_path='/gostream/mount'; $changed=$true }; if($flavor -eq 'jellyfin'){ $desiredMetrics=8097; if($null -eq $obj.metrics_port -or [int]$obj.metrics_port -ne $desiredMetrics){ $obj.metrics_port=$desiredMetrics; $changed=$true } }; if($flavor -eq 'plex'){ $desiredMetrics=8096; if($null -eq $obj.metrics_port -or [int]$obj.metrics_port -ne $desiredMetrics){ $obj.metrics_port=$desiredMetrics; $changed=$true } }; $bak=''; if($changed -and $flavor -eq 'jellyfin'){ New-Item -ItemType Directory -Force -Path $backupDir | Out-Null; $ts=Get-Date -Format 'yyyyMMdd-HHmmss'; $bak=Join-Path $backupDir ('config.json.'+$ts+'.bak'); Copy-Item -LiteralPath $cfg -Destination $bak -Force }; if($changed){ ($obj | ConvertTo-Json -Depth 64) | Set-Content -LiteralPath $cfg -Encoding UTF8 }; Write-Output ('CONFIG_CHANGED=' + ($(if($changed){'1'}else{'0'}))); Write-Output ('CONFIG_BACKUP=' + $bak); Write-Output ('GOSTREAM_METRICS_PORT=' + ([int]$obj.metrics_port)) }"`) do (
  if /i "%%A"=="CONFIG_CHANGED" set "CONFIG_CHANGED=%%B"
  if /i "%%A"=="CONFIG_BACKUP" set "CONFIG_BACKUP=%%B"
  if /i "%%A"=="GOSTREAM_METRICS_PORT" set "GOSTREAM_METRICS_PORT=%%B"
)

set "__CFG="
set "__BAK="
set "__FLAVOR="

 exit /b 0

:generate_stack
set "_flavor=%~1"
set "_stackDir=%~2"
set "_baseFwd=%~3"
set "_stackFwd=%~4"
set "_container=%~5"
set "_useExisting=%~6"
set "_existingCompose=%~7"
set "_existingDockerfile=%~8"

REM compose.yaml
echo [stack] Writing compose.yaml
if "%_useExisting%"=="1" (
  copy /y "%_existingCompose%" "%_stackDir%\compose.yaml" >nul
) else (
  copy /y "%TEMPLATES_DIR%\compose.yaml.tmpl" "%_stackDir%\compose.yaml" >nul
)
if errorlevel 1 (
  echo ERROR: Failed to write compose.yaml
  exit /b 1
)

REM Dockerfile
echo [stack] Writing Dockerfile
if "%_useExisting%"=="1" (
  copy /y "%_existingDockerfile%" "%_stackDir%\Dockerfile" >nul
) else (
  if /i "%_flavor%"=="plex" (
    copy /y "%TEMPLATES_DIR%\Dockerfile.plex.tmpl" "%_stackDir%\Dockerfile" >nul
  ) else (
    copy /y "%TEMPLATES_DIR%\Dockerfile.jellyfin.tmpl" "%_stackDir%\Dockerfile" >nul
  )
)
if errorlevel 1 (
  echo ERROR: Failed to write Dockerfile
  exit /b 1
)

REM custom-* folders
echo [stack] Copying custom-services
robocopy "%TEMPLATES_DIR%\custom-services" "%_stackDir%\custom-services" /E /R:2 /W:2 /NFL /NDL /NJH /NJS /NP
if errorlevel 8 (
  echo ERROR: Failed to copy custom-services templates
  exit /b 1
)

echo [stack] Copying custom-cont-init
robocopy "%TEMPLATES_DIR%\custom-cont-init" "%_stackDir%\custom-cont-init" /E /R:2 /W:2 /NFL /NDL /NJH /NJS /NP
if errorlevel 8 (
  echo ERROR: Failed to copy custom-cont-init templates
  exit /b 1
)

REM Build context: src\ (minimal copy to reduce path-length issues)
echo [stack] Preparing src build context
if exist "%_stackDir%\src" rmdir /s /q "%_stackDir%\src"
call :ensure_dir "%_stackDir%\src"

REM Root Go package files
echo [stack] Copying root Go files
robocopy "%REPO_ROOT%" "%_stackDir%\src" *.go /LEV:1 /R:2 /W:2 /NFL /NDL /NJH /NJS /NP
set "_rc=%ERRORLEVEL%"
echo [stack] root Go copy rc=%_rc%
if %_rc% GEQ 8 (
  echo ERROR: Failed to copy root Go files into stack src\ ; robocopy exit code %_rc%
  exit /b 1
)

REM Required root non-Go files
echo [stack] Copying required root files
if not exist "%REPO_ROOT%\go.mod" (
  echo ERROR: Missing required source file %REPO_ROOT%\go.mod
  exit /b 1
)
copy /y "%REPO_ROOT%\go.mod" "%_stackDir%\src\go.mod" >nul
if errorlevel 1 (
  echo ERROR: Failed to copy go.mod
  exit /b 1
)

if not exist "%REPO_ROOT%\go.sum" (
  echo ERROR: Missing required source file %REPO_ROOT%\go.sum
  exit /b 1
)
copy /y "%REPO_ROOT%\go.sum" "%_stackDir%\src\go.sum" >nul
if errorlevel 1 (
  echo ERROR: Failed to copy go.sum
  exit /b 1
)

if not exist "%REPO_ROOT%\config.json.example" (
  echo ERROR: Missing required source file %REPO_ROOT%\config.json.example
  exit /b 1
)
copy /y "%REPO_ROOT%\config.json.example" "%_stackDir%\src\config.json.example" >nul
if errorlevel 1 (
  echo ERROR: Failed to copy config.json.example
  exit /b 1
)

if not exist "%REPO_ROOT%\requirements.txt" (
  echo ERROR: Missing required source file %REPO_ROOT%\requirements.txt
  exit /b 1
)
copy /y "%REPO_ROOT%\requirements.txt" "%_stackDir%\src\requirements.txt" >nul
if errorlevel 1 (
  echo ERROR: Failed to copy requirements.txt
  exit /b 1
)

if exist "%REPO_ROOT%\settings.html" (
  copy /y "%REPO_ROOT%\settings.html" "%_stackDir%\src\settings.html" >nul
  if errorlevel 1 (
    echo ERROR: Failed to copy settings.html
    exit /b 1
  )
)

REM Internal packages (exclude heavy testdata trees)
echo [stack] Copying internal packages
robocopy "%REPO_ROOT%\internal" "%_stackDir%\src\internal" /E /XD ".git" "testdata" ".tmp" /R:2 /W:2 /NFL /NDL /NJH /NJS /NP
set "_rc=%ERRORLEVEL%"
echo [stack] internal copy rc=%_rc%
if %_rc% GEQ 8 (
  echo ERROR: Failed to copy internal\ into stack src\internal ; robocopy exit code %_rc%
  echo Hint: Try a shorter --stacks path, for example C:\Dockge\stacks, and re-run.
  exit /b 1
)

REM Python scripts + optional ai package (for gostream import graph)
echo [stack] Copying scripts
robocopy "%REPO_ROOT%\scripts" "%_stackDir%\src\scripts" /E /R:2 /W:2 /NFL /NDL /NJH /NJS /NP
set "_rc=%ERRORLEVEL%"
echo [stack] scripts copy rc=%_rc%
if %_rc% GEQ 8 (
  echo ERROR: Failed to copy scripts\ into stack src\scripts ; robocopy exit code %_rc%
  exit /b 1
)

if exist "%REPO_ROOT%\ai" (
  echo [stack] Copying ai Go files
  robocopy "%REPO_ROOT%\ai" "%_stackDir%\src\ai" *.go /E /R:2 /W:2 /NFL /NDL /NJH /NJS /NP
  set "_rc=%ERRORLEVEL%"
  echo [stack] ai copy rc=%_rc%
  if %_rc% GEQ 8 (
    echo ERROR: Failed to copy ai\ Go files into stack src\ai ; robocopy exit code %_rc%
    exit /b 1
  )
)

 REM Render .env (concrete values)
 echo [stack] Writing .env
 (
   echo BASE_DIR=%_baseFwd%
   echo STACK_DIR=%_stackFwd%
   echo CONTAINER_NAME=%_container%
   echo.
   echo MEDIA_HOST_PORT=%MEDIA_HOST_PORT%
   echo MEDIA_CONTAINER_PORT=%MEDIA_CONTAINER_PORT%
   echo.
   echo GOSTREAM_METRICS_PORT=%GOSTREAM_METRICS_PORT%
   echo.
   echo PUID=%PUID%
   echo PGID=%PGID%
   echo TZ=%TZ%
   echo PLEX_CLAIM=%PLEX_CLAIM%
 ) > "%_stackDir%\.env"

exit /b 0

:do_plex_import
set "_src=%~1"
set "_dest=%~2"

echo.
echo WARNING: Windows Plex plugins/scanners may not work on Linux containers.

if not exist "%_src%" (
  echo ERROR: Plex import source does not exist: %_src%
  exit /b 1
)

call :ensure_dir "%_dest%"

REM If destination exists and is non-empty, back it up
set "_hasFiles=0"
for /f "delims=" %%I in ('dir /a /b "%_dest%" 2^>nul') do (
  set "_hasFiles=1"
  goto plex_dest_checked
)
:plex_dest_checked

if "%_hasFiles%"=="1" (
  for /f "usebackq delims=" %%T in (`powershell -NoProfile -ExecutionPolicy Bypass -Command "Get-Date -Format yyyyMMdd-HHmmss"`) do set "_ts=%%T"
  set "_backupBase=%BASE_DIR_WIN%\gostream-plex\config-backup\Plex Media Server.%_ts%"
  call :ensure_dir "%_backupBase%"
  echo Backing up existing destination to:
  echo   %_backupBase%
  robocopy "%_dest%" "%_backupBase%" /E /R:2 /W:2 /NFL /NDL /NJH /NJS /NP >nul
  if errorlevel 8 (
    echo ERROR: Backup copy failed.
    exit /b 1
  )
)

echo Importing Plex data:
echo   from: %_src%
echo   to  : %_dest%
robocopy "%_src%" "%_dest%" /E /R:2 /W:2 /NFL /NDL /NJH /NJS /NP
set "_rc=%ERRORLEVEL%"

if %_rc% GEQ 8 (
  echo.
  echo ERROR: Plex import failed (robocopy exit code %_rc%).
  echo Guidance:
  echo   - Stop Plex on Windows (to unlock files)
  echo   - Re-run this installer
  exit /b 1
)

exit /b 0

:deploy_stack
set "_stackDir=%~1"

where docker >nul 2>&1
if errorlevel 1 (
  echo ERROR: docker.exe not found in PATH.
  exit /b 1
)

pushd "%_stackDir%" >nul

echo.
echo Deploying stack via Docker Compose...
docker compose --env-file .env -f compose.yaml up -d --build --force-recreate
set "_rc=%ERRORLEVEL%"
popd >nul

if not "%_rc%"=="0" (
  echo ERROR: docker compose failed with exit code %_rc%.
  exit /b %_rc%
)

exit /b 0

:deploy_existing_compose
set "_composePath=%~1"
for %%I in ("%_composePath%") do (
  set "_composeDir=%%~dpI"
  set "_composeFile=%%~nxI"
)

where docker >nul 2>&1
if errorlevel 1 (
  echo ERROR: docker.exe not found in PATH.
  exit /b 1
)

pushd "%_composeDir%" >nul
echo.
echo Deploying existing compose via Docker Compose...
echo   Compose file: %_composeFile%
docker compose -f "%_composeFile%" up -d --build --force-recreate
set "_rc=%ERRORLEVEL%"
popd >nul

if not "%_rc%"=="0" (
  echo ERROR: docker compose failed with exit code %_rc%.
  exit /b %_rc%
)

exit /b 0

:materialize_mydeploy_stack
set "_srcCompose=%~1"
set "_stackDir=%~2"
set "_repoRoot=%~3"
set "_outVar=%~4"

call :ensure_dir "%_stackDir%"

set "_matScript=%SCRIPT_DIR%scripts\materialize-mydeploy-compose.ps1"
set "_dstCompose=%_stackDir%\compose.yaml"
set "_MAT_COMPOSE="

if not exist "%_matScript%" (
  echo ERROR: Missing materialize script: %_matScript%
  exit /b 1
)

for /f "usebackq tokens=1* delims==" %%A in (`powershell -NoProfile -ExecutionPolicy Bypass -File "%_matScript%" -SourceCompose "%_srcCompose%" -DestCompose "%_dstCompose%" -RepoRoot "%_repoRoot%"`) do (
  if /i "%%A"=="MATERIALIZED_COMPOSE" set "_MAT_COMPOSE=%%B"
)

if "%_MAT_COMPOSE%"=="" (
  echo ERROR: Failed to materialize my-deploy compose into stack folder.
  exit /b 1
)

set "%_outVar%=%_MAT_COMPOSE%"
exit /b 0


:ensure_dir
set "_d=%~1"
if "%_d%"=="" exit /b 1
if not exist "%_d%" mkdir "%_d%" >nul 2>&1
exit /b 0

; Compile with makensis /DSTAGE_DIR=<prepared directory> /DOUTPUT_FILE=<exe> /DAPP_VERSION=0.1.0 windows.nsi
Unicode True
!include "MUI2.nsh"
!include "x64.nsh"
!include "WinVer.nsh"
!ifndef STAGE_DIR
  !error "STAGE_DIR is required; use scripts/package.ps1"
!endif
!ifndef OUTPUT_FILE
  !error "OUTPUT_FILE is required"
!endif
!ifndef UNINSTALL_LIST
  !error "UNINSTALL_LIST is required; use scripts/package.ps1"
!endif
!ifndef APP_VERSION
  !define APP_VERSION "0.1.0"
!endif
Name "uart2llm"
OutFile "${OUTPUT_FILE}"
InstallDir "$LOCALAPPDATA\Programs\uart2llm"
InstallDirRegKey HKCU "Software\uart2llm" "InstallDir"
RequestExecutionLevel user
SetCompressor /SOLID lzma
SetCompressorDictSize 32
BrandingText "uart2llm - Windows / ESP32-S3"
ShowInstDetails show
ShowUninstDetails show
!define MUI_ABORTWARNING
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!define MUI_FINISHPAGE_TEXT "Installation complete. Use the Start Menu to open uart2llm. The proxy starts only when you request it; no service or startup task was installed. Offline firmware provisioning and flashing tools are included; see README.md."
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Function .onInit
  ${IfNot} ${RunningX64}
    MessageBox MB_ICONSTOP "uart2llm requires 64-bit Windows 10 or later."
    Abort
  ${EndIf}
  ${IfNot} ${AtLeastWin10}
    MessageBox MB_ICONSTOP "uart2llm requires Windows 10 or later."
    Abort
  ${EndIf}
  FindWindow $0 "" "uart2llm - ESP32-S3 Control Console"
  ${If} $0 != 0
    MessageBox MB_ICONEXCLAMATION "Close the uart2llm GUI before installing or updating."
    Abort
  ${EndIf}
  SetShellVarContext current
FunctionEnd

Function un.onInit
  FindWindow $0 "" "uart2llm - ESP32-S3 Control Console"
  ${If} $0 != 0
    MessageBox MB_ICONEXCLAMATION "Close the uart2llm GUI before uninstalling."
    Abort
  ${EndIf}
FunctionEnd

Section "uart2llm" SEC_MAIN
  SetShellVarContext current
  IfFileExists "$INSTDIR\uart2llm.exe" 0 +3
    nsExec::ExecToLog /TIMEOUT=10000 '"$INSTDIR\uart2llm.exe" stop'
    Pop $0
  SetOutPath "$INSTDIR"
  File /r "${STAGE_DIR}/*"
  WriteUninstaller "$INSTDIR\Uninstall.exe"
  CreateDirectory "$SMPROGRAMS\uart2llm"
  CreateShortcut "$SMPROGRAMS\uart2llm\uart2llm.lnk" "$INSTDIR\uart2llm-gui.exe"
  CreateShortcut "$SMPROGRAMS\uart2llm\User guide.lnk" "$SYSDIR\notepad.exe" '"$INSTDIR\README.md"'
  WriteRegStr HKCU "Software\uart2llm" "InstallDir" "$INSTDIR"
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\uart2llm" "DisplayName" "uart2llm"
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\uart2llm" "DisplayVersion" "${APP_VERSION}"
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\uart2llm" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\uart2llm" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegDWORD HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\uart2llm" "NoModify" 1
  WriteRegDWORD HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\uart2llm" "NoRepair" 1
SectionEnd

Section "Uninstall"
  SetShellVarContext current
  nsExec::ExecToLog /TIMEOUT=10000 '"$INSTDIR\uart2llm.exe" stop'
  Pop $0
  ; Exact generated file list and empty-directory removals preserve user-added files.
  !include "${UNINSTALL_LIST}"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\uart2llm\uart2llm.lnk"
  Delete "$SMPROGRAMS\uart2llm\User guide.lnk"
  RMDir "$SMPROGRAMS\uart2llm"
  DeleteRegKey HKCU "Software\Microsoft\Windows\CurrentVersion\Uninstall\uart2llm"
  DeleteRegKey HKCU "Software\uart2llm"
SectionEnd

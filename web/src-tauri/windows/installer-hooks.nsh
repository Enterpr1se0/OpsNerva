!macro STOP_OPSNERVA_SIDECAR
  !if "${INSTALLMODE}" == "currentUser"
    nsis_tauri_utils::KillProcessCurrentUser "ops-agent.exe"
  !else
    nsis_tauri_utils::KillProcess "ops-agent.exe"
  !endif
  Pop $R0
  Sleep 500
  ${If} $R0 != 0
  ${AndIf} $R0 != 2
    Abort "Unable to stop ops-agent.exe."
  ${EndIf}
!macroend

!macro NSIS_HOOK_PREINSTALL
  !insertmacro CheckIfAppIsRunning "${MAINBINARYNAME}.exe" "${PRODUCTNAME}"
  !insertmacro STOP_OPSNERVA_SIDECAR
!macroend

!macro NSIS_HOOK_PREUNINSTALL
  !insertmacro CheckIfAppIsRunning "${MAINBINARYNAME}.exe" "${PRODUCTNAME}"
  !insertmacro STOP_OPSNERVA_SIDECAR
!macroend

!macro NSIS_HOOK_POSTUNINSTALL
  ${If} $DeleteAppDataCheckboxState = 1
  ${AndIf} $UpdateMode <> 1
    Delete /REBOOTOK "$INSTDIR\config.yaml"
    RMDir /r /REBOOTOK "$INSTDIR\data"
    RMDir /r /REBOOTOK "$INSTDIR\.data"
    RMDir /r /REBOOTOK "$INSTDIR\workspace"
    RMDir /REBOOTOK "$INSTDIR"
  ${EndIf}
!macroend

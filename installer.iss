; Инсталлятор ClickHouse Show Polygons (Inno Setup)
; Версия передаётся из CI: ISCC /DAppVersion=1.0.0 installer.iss

#ifndef AppVersion
  #define AppVersion "0.0.0"
#endif

[Setup]
AppId={{7E2B0C71-4E8A-4D2B-9B1E-CH0WP0LYG0NS}
AppName=ClickHouse Show Polygons
AppVersion={#AppVersion}
AppPublisher=akprof2000
AppPublisherURL=https://github.com/akprof2000/ClickHouse-Show-Polygons
DefaultDirName={autopf}\ClickHouse Show Polygons
DefaultGroupName=ClickHouse Show Polygons
UninstallDisplayIcon={app}\chviewer.exe
OutputBaseFilename=ClickHouse-Show-Polygons-Setup-{#AppVersion}
Compression=lzma2
SolidCompression=yes
PrivilegesRequired=lowest
DisableProgramGroupPage=yes

[Languages]
Name: "russian"; MessagesFile: "compiler:Languages\Russian.isl"

[Files]
Source: "chviewer.exe"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\ClickHouse Show Polygons"; Filename: "{app}\chviewer.exe"
Name: "{autodesktop}\ClickHouse Show Polygons"; Filename: "{app}\chviewer.exe"; Tasks: desktopicon

[Tasks]
Name: "desktopicon"; Description: "Создать ярлык на рабочем столе"; GroupDescription: "Дополнительно:"

[Run]
Filename: "{app}\chviewer.exe"; Description: "Запустить ClickHouse Show Polygons"; Flags: nowait postinstall skipifsilent

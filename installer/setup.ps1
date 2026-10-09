# SWRemote Setup — WPF installer wizard (install / update / uninstall)
# Run: powershell -ExecutionPolicy Bypass -File setup.ps1 [-Uninstall]
param([switch]$Uninstall)

Add-Type -AssemblyName PresentationFramework
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing

$AppName    = "SWRemote"
$Publisher  = "SWInfoSystems"
$Version    = "3.0.0"
$ExeName    = "SWRemote-Agent.exe"
$InstallDir = Join-Path $env:LOCALAPPDATA "SWRemote"
$SrcExe     = Join-Path $PSScriptRoot $ExeName
$IsUpdate   = (Test-Path (Join-Path $InstallDir $ExeName)) -and (-not $Uninstall)

[xml]$xaml = @"
<Window xmlns="http://schemas.microsoft.com/winfx/2006/xaml/presentation"
        xmlns:x="http://schemas.microsoft.com/winfx/2006/xaml"
        Title="SWRemote Setup" Height="600" Width="500"
        WindowStyle="None" AllowsTransparency="True" Background="Transparent"
        WindowStartupLocation="CenterScreen" ResizeMode="NoResize">
  <Border CornerRadius="20" Background="White" BorderBrush="#E7EDF5" BorderThickness="1">
    <Border.Effect><DropShadowEffect BlurRadius="30" ShadowDepth="0" Opacity="0.18"/></Border.Effect>
    <Grid>
      <Grid.RowDefinitions>
        <RowDefinition Height="120"/>
        <RowDefinition Height="*"/>
        <RowDefinition Height="76"/>
      </Grid.RowDefinitions>

      <!-- header -->
      <Border Grid.Row="0" CornerRadius="20,20,0,0" Name="HeaderBar" Background="#2F7DE1">
        <Border.Background>
          <LinearGradientBrush StartPoint="0,0" EndPoint="1,1">
            <GradientStop Color="#2F7DE1" Offset="0"/>
            <GradientStop Color="#5AA2F5" Offset="1"/>
          </LinearGradientBrush>
        </Border.Background>
        <Grid Margin="26,0,20,0">
          <StackPanel Orientation="Horizontal" VerticalAlignment="Center">
            <Border Width="58" Height="58" CornerRadius="17" Background="White">
              <Image Name="HeadLogo" Stretch="Uniform" Margin="7"/>
            </Border>
            <StackPanel Margin="16,0,0,0" VerticalAlignment="Center">
              <TextBlock Name="HeadTitle" Text="Install SWRemote" FontSize="23" FontWeight="Bold" Foreground="White"/>
              <TextBlock Text="by SWInfoSystems  •  v$Version" FontSize="12" Foreground="#DCE9FB" Margin="0,3,0,0"/>
            </StackPanel>
          </StackPanel>
          <Button Name="BtnX" Content="✕" Width="36" Height="36" FontSize="15"
                  HorizontalAlignment="Right" VerticalAlignment="Top" Margin="0,14,0,0"
                  Background="Transparent" Foreground="White" BorderThickness="0" Cursor="Hand"/>
        </Grid>
      </Border>

      <!-- pages -->
      <Grid Grid.Row="1" Margin="34,26,34,10">
        <!-- welcome -->
        <StackPanel Name="PageWelcome" Visibility="Visible">
          <Border CornerRadius="14" Margin="0,0,0,6" Background="#F4F6FA">
            <Image Name="WelcomeArt" Stretch="UniformToFill" Height="150"/>
            <Border.Clip><RectangleGeometry Rect="0,0,420,150" RadiusX="14" RadiusY="14"/></Border.Clip>
          </Border>
          <TextBlock Name="WelcomeHead" Text="Welcome!" FontSize="20" FontWeight="Bold" Foreground="#16233A" Margin="0,8,0,0"/>
          <TextBlock Name="WelcomeText" FontSize="14" Foreground="#5A6B85" Margin="0,10,0,0" TextWrapping="Wrap" LineHeight="24"/>
          <Border Background="#E9F1FD" CornerRadius="12" Padding="16" Margin="0,22,0,0">
            <StackPanel>
              <TextBlock Text="What gets installed" FontWeight="Bold" FontSize="13" Foreground="#1F5FC0"/>
              <TextBlock Text="• SWRemote agent — shares this PC's screen&#x0a;• Start Menu &amp; desktop shortcuts&#x0a;• Optional: start automatically with Windows&#x0a;• No admin rights needed" FontSize="13" Foreground="#3C4C66" Margin="0,8,0,0" LineHeight="22"/>
            </StackPanel>
          </Border>
        </StackPanel>

        <!-- options -->
        <StackPanel Name="PageOptions" Visibility="Collapsed">
          <TextBlock Text="Install options" FontSize="20" FontWeight="Bold" Foreground="#16233A"/>
          <TextBlock Text="INSTALL FOLDER" FontSize="11" FontWeight="Bold" Foreground="#8494AB" Margin="0,18,0,6"/>
          <Grid>
            <Grid.ColumnDefinitions><ColumnDefinition Width="*"/><ColumnDefinition Width="90"/></Grid.ColumnDefinitions>
            <TextBox Name="TxtPath" Grid.Column="0" Padding="10" FontSize="13" BorderBrush="#E7EDF5"/>
            <Button Name="BtnBrowse" Grid.Column="1" Content="Browse…" Margin="8,0,0,0" Padding="8"
                    Background="#E9F1FD" Foreground="#1F5FC0" BorderThickness="0" Cursor="Hand"/>
          </Grid>
          <TextBlock Text="SHORTCUTS &amp; STARTUP" FontSize="11" FontWeight="Bold" Foreground="#8494AB" Margin="0,20,0,4"/>
          <CheckBox Name="ChkDesktop" Content="Desktop shortcut" IsChecked="True" FontSize="14" Margin="0,8,0,0" Foreground="#16233A"/>
          <CheckBox Name="ChkStartMenu" Content="Start Menu shortcut" IsChecked="True" FontSize="14" Margin="0,8,0,0" Foreground="#16233A"/>
          <CheckBox Name="ChkStartup" Content="Start SWRemote with Windows" IsChecked="True" FontSize="14" Margin="0,8,0,0" Foreground="#16233A"/>
        </StackPanel>

        <!-- progress -->
        <StackPanel Name="PageProgress" Visibility="Collapsed" VerticalAlignment="Center">
          <TextBlock Name="ProgTitle" Text="Installing…" FontSize="20" FontWeight="Bold" Foreground="#16233A" HorizontalAlignment="Center"/>
          <ProgressBar Name="ProgBar" Height="14" Margin="0,24,0,0" Minimum="0" Maximum="100"/>
          <TextBlock Name="ProgText" Text="" FontSize="13" Foreground="#8494AB" Margin="0,14,0,0" HorizontalAlignment="Center"/>
        </StackPanel>

        <!-- finish -->
        <StackPanel Name="PageFinish" Visibility="Collapsed" VerticalAlignment="Center">
          <Border Width="76" Height="76" CornerRadius="38" Background="#E4F6EC" HorizontalAlignment="Center">
            <TextBlock Text="✓" FontSize="38" FontWeight="Bold" Foreground="#22A35F"
                       HorizontalAlignment="Center" VerticalAlignment="Center"/>
          </Border>
          <TextBlock Name="FinishHead" Text="Installed!" FontSize="22" FontWeight="Bold" Foreground="#16233A"
                     HorizontalAlignment="Center" Margin="0,18,0,0"/>
          <TextBlock Name="FinishText" FontSize="14" Foreground="#5A6B85" TextWrapping="Wrap" TextAlignment="Center"
                     Margin="0,10,0,0" LineHeight="24"/>
        </StackPanel>
      </Grid>

      <!-- footer buttons -->
      <Grid Grid.Row="2" Margin="34,0,34,22">
        <StackPanel Orientation="Horizontal" HorizontalAlignment="Right">
          <Button Name="BtnBack" Content="← Back" Width="96" Height="42" Margin="0,0,10,0" FontSize="14"
                  Background="#EEF1F6" Foreground="#3C4C66" BorderThickness="0" Cursor="Hand" Visibility="Collapsed"/>
          <Button Name="BtnNext" Content="Next →" Width="130" Height="42" FontSize="14" FontWeight="Bold"
                  Foreground="White" BorderThickness="0" Cursor="Hand">
            <Button.Background>
              <LinearGradientBrush StartPoint="0,0" EndPoint="1,0">
                <GradientStop Color="#2F7DE1" Offset="0"/><GradientStop Color="#5AA2F5" Offset="1"/>
              </LinearGradientBrush>
            </Button.Background>
          </Button>
        </StackPanel>
      </Grid>
    </Grid>
  </Border>
</Window>
"@

$reader = New-Object System.Xml.XmlNodeReader $xaml
$win = [Windows.Markup.XamlReader]::Load($reader)
$find = { param($n) $win.FindName($n) }

# drag the borderless window
$win.Add_MouseLeftButtonDown({ $win.DragMove() })
(& $find "BtnX").Add_Click({ $win.Close() })

$pages = @("PageWelcome", "PageOptions", "PageProgress", "PageFinish")
function Show-Page($name) {
  foreach ($p in $pages) { (& $find $p).Visibility = $(if ($p -eq $name) { "Visible" } else { "Collapsed" }) }
}
$BtnBack = & $find "BtnBack"; $BtnNext = & $find "BtnNext"

function Set-Progress($pct, $text) {
  (& $find "ProgBar").Value = $pct
  (& $find "ProgText").Text = $text
  [System.Windows.Forms.Application]::DoEvents() 2>$null
  Start-Sleep -Milliseconds 250
}

function Install-SWRemote($dir, $desktop, $startmenu, $startup) {
  # stop a running copy first (it may live in the target dir)
  Get-Process -Name "SWRemote-Agent" -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
  Start-Sleep -Milliseconds 800

  Show-Page "PageProgress"
  (& $find "ProgTitle").Text = $(if ($IsUpdate) { "Updating…" } else { "Installing…" })
  $BtnBack.Visibility = "Collapsed"; $BtnNext.Visibility = "Collapsed"

  Set-Progress 15 "Preparing files…"
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  Set-Progress 35 "Copying SWRemote agent…"
  Copy-Item -Path $SrcExe -Destination (Join-Path $dir $ExeName) -Force
  "v$Version" | Out-File (Join-Path $dir "version.txt") -Encoding ascii

  Set-Progress 60 "Creating shortcuts…"
  $shell = New-Object -ComObject WScript.Shell
  $target = Join-Path $dir $ExeName
  if ($desktop) {
    $sc = $shell.CreateShortcut((Join-Path ([Environment]::GetFolderPath("Desktop")) "SWRemote.lnk"))
    $sc.TargetPath = $target; $sc.WorkingDirectory = $dir
    $sc.Description = "SWRemote by SWInfoSystems"; $sc.Save()
  } else {
    Remove-Item (Join-Path ([Environment]::GetFolderPath("Desktop")) "SWRemote.lnk") -ErrorAction SilentlyContinue
  }
  $smPath = Join-Path ([Environment]::GetFolderPath("Programs")) "SWRemote.lnk"
  if ($startmenu) {
    $sc = $shell.CreateShortcut($smPath)
    $sc.TargetPath = $target; $sc.WorkingDirectory = $dir
    $sc.Description = "SWRemote by SWInfoSystems"; $sc.Save()
  } else {
    Remove-Item $smPath -ErrorAction SilentlyContinue
  }

  Set-Progress 78 "Configuring startup…"
  $runKey = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run"
  if ($startup) { Set-ItemProperty -Path $runKey -Name $AppName -Value "`"$target`"" }
  else { Remove-ItemProperty -Path $runKey -Name $AppName -ErrorAction SilentlyContinue }

  Set-Progress 90 "Registering uninstaller…"
  $unKey = "HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\$AppName"
  New-Item -Path $unKey -Force | Out-Null
  Set-ItemProperty -Path $unKey -Name "DisplayName" -Value "$AppName by $Publisher"
  Set-ItemProperty -Path $unKey -Name "DisplayVersion" -Value $Version
  Set-ItemProperty -Path $unKey -Name "Publisher" -Value $Publisher
  Set-ItemProperty -Path $unKey -Name "UninstallString" -Value "`"$target`" --uninstall"
  Set-ItemProperty -Path $unKey -Name "NoModify" -Value 1 -Type DWord
  Set-ItemProperty -Path $unKey -Name "NoRepair" -Value 1 -Type DWord

  Set-Progress 100 "Done"
  (& $find "FinishHead").Text = $(if ($IsUpdate) { "Updated!" } else { "Installed!" })
  (& $find "FinishText").Text = $(if ($IsUpdate) {
    "SWRemote is now up to date. Your ID and PIN were kept."
  } else {
    "SWRemote is ready. Open it from the desktop or Start Menu — a window will show your ID and PIN."
  })
  Show-Page "PageFinish"
  $BtnNext.Content = "Launch SWRemote"
  $BtnNext.Visibility = "Visible"
  $script:finished = "launch"
}

function Uninstall-SWRemote {
  Show-Page "PageProgress"
  (& $find "ProgTitle").Text = "Uninstalling…"
  $BtnBack.Visibility = "Collapsed"; $BtnNext.Visibility = "Collapsed"
  Set-Progress 20 "Stopping SWRemote…"
  Get-Process -Name "SWRemote-Agent" -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
  Start-Sleep -Milliseconds 800
  Set-Progress 45 "Removing shortcuts…"
  Remove-Item (Join-Path ([Environment]::GetFolderPath("Desktop")) "SWRemote.lnk") -ErrorAction SilentlyContinue
  Remove-Item (Join-Path ([Environment]::GetFolderPath("Programs")) "SWRemote.lnk") -ErrorAction SilentlyContinue
  Set-Progress 65 "Cleaning registry…"
  Remove-ItemProperty -Path "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" -Name $AppName -ErrorAction SilentlyContinue
  Remove-Item -Path "HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\$AppName" -Recurse -ErrorAction SilentlyContinue
  Set-Progress 85 "Removing files…"
  Remove-Item (Join-Path $InstallDir $ExeName) -Force -ErrorAction SilentlyContinue
  Remove-Item (Join-Path $InstallDir "version.txt") -Force -ErrorAction SilentlyContinue
  Remove-Item (Join-Path $InstallDir "swremote.json") -Force -ErrorAction SilentlyContinue
  # remove dir if empty
  if ((Get-ChildItem $InstallDir -ErrorAction SilentlyContinue | Measure-Object).Count -eq 0) {
    Remove-Item $InstallDir -Force -ErrorAction SilentlyContinue
  }
  Set-Progress 100 "Done"
  (& $find "FinishHead").Text = "Uninstalled"
  (& $find "FinishText").Text = "SWRemote has been removed from this PC."
  Show-Page "PageFinish"
  $BtnNext.Content = "Close"
  $BtnNext.Visibility = "Visible"
  $script:finished = "close"
}

# ---------- wire up ----------
$script:stage = "welcome"
$script:finished = ""

function Set-Img($name, $file) {
  $p = Join-Path $PSScriptRoot $file
  if (Test-Path $p) {
    $bmp = New-Object System.Windows.Media.Imaging.BitmapImage
    $bmp.BeginInit()
    $bmp.UriSource = New-Object System.Uri($p)
    $bmp.CacheOption = [System.Windows.Media.Imaging.BitmapCacheOption]::OnLoad
    $bmp.EndInit()
    (& $find $name).Source = $bmp
  }
}
Set-Img "HeadLogo" "logo-mark.png"
Set-Img "WelcomeArt" "install-art.png"

if ($Uninstall) {
  (& $find "HeadTitle").Text = "Uninstall SWRemote"
  (& $find "WelcomeHead").Text = "Remove SWRemote?"
  (& $find "WelcomeText").Text = "This will remove the SWRemote agent, its shortcuts, startup entry and settings from this PC."
  $BtnNext.Content = "Uninstall"
  $BtnBack.Visibility = "Collapsed"
} elseif ($IsUpdate) {
  (& $find "HeadTitle").Text = "Update SWRemote"
  (& $find "WelcomeHead").Text = "Update available"
  (& $find "WelcomeText").Text = "SWRemote is already installed on this PC. This will update it to v$Version — your ID and PIN stay the same."
  $BtnNext.Content = "Update"
} else {
  (& $find "WelcomeText").Text = "This will install the SWRemote agent on this PC so you can share your screen and receive remote help. No admin rights needed, takes less than a minute."
  (& $find "TxtPath").Text = $InstallDir
}

$BtnBack.Add_Click({
  if ($script:stage -eq "options") { $script:stage = "welcome"; Show-Page "PageWelcome"; $BtnBack.Visibility = "Collapsed" }
})

$BtnNext.Add_Click({
  if ($script:finished -eq "launch") { Start-Process (Join-Path $script:finalDir $ExeName); $win.Close(); return }
  if ($script:finished -eq "close") { $win.Close(); return }
  if ($Uninstall) { Uninstall-SWRemote; return }
  if ($script:stage -eq "welcome") {
    if ($IsUpdate) {
      $script:finalDir = $InstallDir
      Install-SWRemote $InstallDir $true $true $true
    } else {
      $script:stage = "options"; Show-Page "PageOptions"; $BtnBack.Visibility = "Visible"
      $BtnNext.Content = "Install"
    }
    return
  }
  if ($script:stage -eq "options") {
    $dir = (& $find "TxtPath").Text.Trim()
    if (-not $dir) { $dir = $InstallDir }
    $script:finalDir = $dir
    Install-SWRemote $dir (& $find "ChkDesktop").IsChecked (& $find "ChkStartMenu").IsChecked (& $find "ChkStartup").IsChecked
  }
})

(& $find "BtnBrowse").Add_Click({
  $dlg = New-Object System.Windows.Forms.FolderBrowserDialog
  $dlg.Description = "Choose install folder"
  $dlg.SelectedPath = (& $find "TxtPath").Text
  if ($dlg.ShowDialog() -eq "OK") { (& $find "TxtPath").Text = $dlg.SelectedPath }
})

[void]$win.ShowDialog()

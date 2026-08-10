package main

import (
	"fmt"
	"os"
	"strings"
)

// ==================== 命令行 i18n ====================
// 语言优先级：TSUMUGI_LANG > LC_ALL > LC_MESSAGES > LANG；未知语言回退中文。
// 支持与服务端一致的 10 种语言。

var cliLangList = []string{"zh", "en", "ja", "ko", "fr", "de", "es", "pt", "ru", "vi"}

// cliDict 命令行文案字典：key -> lang -> 文本。
var cliDict = map[string]map[string]string{
	"cliTitle": {
		"zh": "Tsumugi CLI - Tsumugi 命令行管理工具",
		"en": "Tsumugi CLI - command-line management tool",
		"ja": "Tsumugi CLI - コマンドライン管理ツール",
		"ko": "Tsumugi CLI - 명령줄 관리 도구",
		"fr": "Tsumugi CLI - outil d'administration en ligne de commande",
		"de": "Tsumugi CLI - Befehlszeilen-Verwaltungstool",
		"es": "Tsumugi CLI - herramienta de administración por línea de comandos",
		"pt": "Tsumugi CLI - ferramenta de administração por linha de comandos",
		"ru": "Tsumugi CLI - инструмент управления из командной строки",
		"vi": "Tsumugi CLI - công cụ quản trị dòng lệnh",
	},
	"usageHead": {
		"zh": "用法:",
		"en": "Usage:",
		"ja": "使い方:",
		"ko": "사용법:",
		"fr": "Usage :",
		"de": "Verwendung:",
		"es": "Uso:",
		"pt": "Uso:",
		"ru": "Использование:",
		"vi": "Cách dùng:",
	},
	"argsHead": {
		"zh": "参数:",
		"en": "Options:",
		"ja": "オプション:",
		"ko": "옵션:",
		"fr": "Options :",
		"de": "Optionen:",
		"es": "Opciones:",
		"pt": "Opções:",
		"ru": "Параметры:",
		"vi": "Tùy chọn:",
	},
	"optHost": {
		"zh": "服务器地址（默认 127.0.0.1，环境变量 TSUMUGI_HOST）",
		"en": "server address (default 127.0.0.1, env TSUMUGI_HOST)",
		"ja": "サーバーアドレス（既定 127.0.0.1、環境変数 TSUMUGI_HOST）",
		"ko": "서버 주소 (기본 127.0.0.1, 환경 변수 TSUMUGI_HOST)",
		"fr": "adresse du serveur (défaut 127.0.0.1, variable TSUMUGI_HOST)",
		"de": "Serveradresse (Standard 127.0.0.1, Umgebungsvariable TSUMUGI_HOST)",
		"es": "dirección del servidor (por defecto 127.0.0.1, variable TSUMUGI_HOST)",
		"pt": "endereço do servidor (padrão 127.0.0.1, var TSUMUGI_HOST)",
		"ru": "адрес сервера (по умолчанию 127.0.0.1, переменная TSUMUGI_HOST)",
		"vi": "địa chỉ máy chủ (mặc định 127.0.0.1, biến môi trường TSUMUGI_HOST)",
	},
	"optPort": {
		"zh": "二进制协议端口（默认 9999，环境变量 TSUMUGI_PORT）",
		"en": "binary protocol port (default 9999, env TSUMUGI_PORT)",
		"ja": "バイナリプロトコルポート（既定 9999、環境変数 TSUMUGI_PORT）",
		"ko": "바이너리 프로토콜 포트 (기본 9999, 환경 변수 TSUMUGI_PORT)",
		"fr": "port du protocole binaire (défaut 9999, variable TSUMUGI_PORT)",
		"de": "Port des Binärprotokolls (Standard 9999, Umgebungsvariable TSUMUGI_PORT)",
		"es": "puerto del protocolo binario (por defecto 9999, variable TSUMUGI_PORT)",
		"pt": "porta do protocolo binário (padrão 9999, var TSUMUGI_PORT)",
		"ru": "порт бинарного протокола (по умолчанию 9999, переменная TSUMUGI_PORT)",
		"vi": "cổng giao thức nhị phân (mặc định 9999, biến TSUMUGI_PORT)",
	},
	"optUser": {
		"zh": "用户名（默认 root，环境变量 TSUMUGI_USER）",
		"en": "username (default root, env TSUMUGI_USER)",
		"ja": "ユーザー名（既定 root、環境変数 TSUMUGI_USER）",
		"ko": "사용자 이름 (기본 root, 환경 변수 TSUMUGI_USER)",
		"fr": "nom d'utilisateur (défaut root, variable TSUMUGI_USER)",
		"de": "Benutzername (Standard root, Umgebungsvariable TSUMUGI_USER)",
		"es": "usuario (por defecto root, variable TSUMUGI_USER)",
		"pt": "utilizador (padrão root, var TSUMUGI_USER)",
		"ru": "имя пользователя (по умолчанию root, переменная TSUMUGI_USER)",
		"vi": "tên người dùng (mặc định root, biến TSUMUGI_USER)",
	},
	"optPass": {
		"zh": "密码（默认 password，环境变量 TSUMUGI_PASS）",
		"en": "password (default password, env TSUMUGI_PASS)",
		"ja": "パスワード（既定 password、環境変数 TSUMUGI_PASS）",
		"ko": "비밀번호 (기본 password, 환경 변수 TSUMUGI_PASS)",
		"fr": "mot de passe (défaut password, variable TSUMUGI_PASS)",
		"de": "Passwort (Standard password, Umgebungsvariable TSUMUGI_PASS)",
		"es": "contraseña (por defecto password, variable TSUMUGI_PASS)",
		"pt": "palavra-passe (padrão password, var TSUMUGI_PASS)",
		"ru": "пароль (по умолчанию password, переменная TSUMUGI_PASS)",
		"vi": "mật khẩu (mặc định password, biến TSUMUGI_PASS)",
	},
	"optExec": {
		"zh": "执行单条 SQL 后退出（支持分号分隔多条）",
		"en": "run a SQL statement and exit (semicolon separates multiple)",
		"ja": "SQLを1つ実行して終了（セミコロン区切りで複数可）",
		"ko": "SQL 하나를 실행하고 종료 (세미콜론으로 여러 개 가능)",
		"fr": "exécuter une requête SQL puis quitter (séparée par des points-virgules)",
		"de": "SQL ausführen und beenden (mehrere durch Semikolon getrennt)",
		"es": "ejecutar una sentencia SQL y salir (punto y coma separa varias)",
		"pt": "executa um SQL e termina (ponto e vírgula separa vários)",
		"ru": "выполнить SQL и выйти (точки с запятой разделяют несколько)",
		"vi": "chạy một câu SQL rồi thoát (chấm phẩy ngăn cách nhiều câu)",
	},
	"optFile": {
		"zh": "执行 SQL 脚本文件后退出",
		"en": "run a SQL script file and exit",
		"ja": "SQLスクリプトファイルを実行して終了",
		"ko": "SQL 스크립트 파일 실행 후 종료",
		"fr": "exécuter un fichier de script SQL puis quitter",
		"de": "SQL-Skriptdatei ausführen und beenden",
		"es": "ejecutar un archivo de script SQL y salir",
		"pt": "executar um ficheiro de script SQL e sair",
		"ru": "выполнить SQL-скрипт из файла и завершить",
		"vi": "chạy file script SQL rồi thoát",
	},
	"optHelp": {
		"zh": "显示帮助",
		"en": "show this help",
		"ja": "ヘルプを表示",
		"ko": "도움말 표시",
		"fr": "afficher l'aide",
		"de": "Hilfe anzeigen",
		"es": "mostrar la ayuda",
		"pt": "mostrar a ajuda",
		"ru": "показать справку",
		"vi": "hiển thị trợ giúp",
	},
	"builtins": {
		"zh": "交互模式内建命令：help / exit / status / compact / backup /",
		"en": "REPL built-in commands: help / exit / status / compact / backup /",
		"ja": "対話モード内蔵コマンド：help / exit / status / compact / backup /",
		"ko": "대화형 내장 명령: help / exit / status / compact / backup /",
		"fr": "Commandes intégrées du REPL : help / exit / status / compact / backup /",
		"de": "Eingebaute REPL-Befehle: help / exit / status / compact / backup /",
		"es": "Comandos integrados de REPL: help / exit / status / compact / backup /",
		"pt": "Comandos integrados do REPL: help / exit / status / compact / backup /",
		"ru": "Встроенные команды: help / exit / status / compact / backup /",
		"vi": "Lệnh tích hợp: help / exit / status / compact / backup /",
	},
	"builtins2": {
		"zh": "  set durability <batch|fsync> / import --table T --file F.csv [--db D]",
		"en": "  set durability <batch|fsync> / import --table T --file F.csv [--db D]",
		"ja": "  set durability <batch|fsync> / import --table T --file F.csv [--db D]",
		"ko": "  set durability <batch|fsync> / import --table T --file F.csv [--db D]",
		"fr": "  set durability <batch|fsync> / import --table T --file F.csv [--db D]",
		"de": "  set durability <batch|fsync> / import --table T --file F.csv [--db D]",
		"es": "  set durability <batch|fsync> / import --table T --file F.csv [--db D]",
		"pt": "  set durability <batch|fsync> / import --table T --file F.csv [--db D]",
		"ru": "  set durability <batch|fsync> / import --table T --file F.csv [--db D]",
		"vi": "  set durability <batch|fsync> / import --table T --file F.csv [--db D]",
	},
	"dbmgmt": {
		"zh": "数据库管理：SHOW DATABASES / CREATE DATABASE / DROP DATABASE / USE <db>",
		"en": "Database management: SHOW DATABASES / CREATE DATABASE / DROP DATABASE / USE <db>",
		"ja": "データベース管理：SHOW DATABASES / CREATE DATABASE / DROP DATABASE / USE <db>",
		"ko": "데이터베이스 관리: SHOW DATABASES / CREATE DATABASE / DROP DATABASE / USE <db>",
		"fr": "Gestion de bases : SHOW DATABASES / CREATE DATABASE / DROP DATABASE / USE <db>",
		"de": "Datenbankverwaltung: SHOW DATABASES / CREATE DATABASE / DROP DATABASE / USE <db>",
		"es": "Gestión de bases: SHOW DATABASES / CREATE DATABASE / DROP DATABASE / USE <db>",
		"pt": "Gestão de bases : SHOW DATABASES / CREATE DATABASE / DROP DATABASE / USE <db>",
		"ru": "Управление БД: SHOW DATABASES / CREATE DATABASE / DROP DATABASE / USE <db>",
		"vi": "Quản lý CSDL: SHOW DATABASES / CREATE DATABASE / DROP DATABASE / USE <db>",
	},
	"welcome": {
		"zh": "欢迎使用 Tsumugi CLI\n输入 'help' 或 'exit' 开始。\n\n",
		"en": "Welcome to Tsumugi CLI\nType 'help' or 'exit'.\n\n",
		"ja": "Tsumugi CLI へようこそ\n'help' か 'exit' と入力してください。\n\n",
		"ko": "Tsumugi CLI에 오신 것을 환영합니다\n'help' 또는 'exit'를 입력하세요.\n\n",
		"fr": "Bienvenue sur Tsumugi CLI\nTapez 'help' ou 'exit'.\n\n",
		"de": "Willkommen bei Tsumugi CLI\nGeben Sie 'help' oder 'exit' ein.\n\n",
		"es": "Bienvenido a Tsumugi CLI\nEscriba 'help' o 'exit'.\n\n",
		"pt": "Bem-vindo ao Tsumugi CLI\nDigite 'help' ou 'exit'.\n\n",
		"ru": "Добро пожаловать в Tsumugi CLI\nВведите 'help' или 'exit'.\n\n",
		"vi": "Chào mừng đến Tsumugi CLI\nGõ 'help' hoặc 'exit'.\n\n",
	},
	"bye": {
		"zh": "再见",
		"en": "Bye",
		"ja": "さようなら",
		"ko": "안녕히 가세요",
		"fr": "Au revoir",
		"de": "Auf Wiedersehen",
		"es": "Adiós",
		"pt": "Adeus",
		"ru": "До свидания",
		"vi": "Tạm biệt",
	},
	"helpHead": {
		"zh": "内建命令（Built-in commands）:",
		"en": "Commands:",
		"ja": "コマンド:",
		"ko": "명령:",
		"fr": "Commandes :",
		"de": "Befehle:",
		"es": "Comandos:",
		"pt": "Comandos:",
		"ru": "Команды:",
		"vi": "Lệnh:",
	},
	"helpSql": {
		"zh": "执行任意 SQL（SELECT/CREATE/INSERT/USE...）",
		"en": "run any SQL (SELECT/CREATE/INSERT/USE...)",
		"ja": "任意のSQLを実行（SELECT/CREATE/INSERT/USE...）",
		"ko": "모든 SQL 실행 (SELECT/CREATE/INSERT/USE...)",
		"fr": "exécuter tout SQL (SELECT/CREATE/INSERT/USE...)",
		"de": "beliebiges SQL ausführen (SELECT/CREATE/INSERT/USE...)",
		"es": "ejecutar cualquier SQL (SELECT/CREATE/INSERT/USE...)",
		"pt": "execut qualquer SQL (SELECT/CREATE/INSERT/USE...)",
		"ru": "выполнить любой SQL (SELECT/CREATE/INSERT/USE...)",
		"vi": "chạy bất kỳ SQL nào (SELECT/CREATE/INSERT/USE...)",
	},
	"helpStatus": {
		"zh": "显示服务状态（QPS/TPS/内存/磁盘）",
		"en": "show server status (QPS/TPS/memory/disk)",
		"ja": "サーバー状態を表示（QPS/TPS/メモリ/ディスク）",
		"ko": "서버 상태 표시 (QPS/TPS/메모리/디스크)",
		"fr": "afficher l'état du serveur (QPS/TPS/mémoire/disque)",
		"de": "Serverstatus anzeigen (QPS/TPS/Speicher/Disk)",
		"es": "mostrar estado del servidor (QPS/TPS/memoria/disco)",
		"pt": "mostrar estado do servidor (QPS/TPS/memória/disco)",
		"ru": "показать состояние сервера (QPS/TPS/память/диск)",
		"vi": "hiển thị trạng thái máy chủ (QPS/TPS/bộ nhớ/đĩa)",
	},
	"helpCompact": {
		"zh": "触发 WAL 整理",
		"en": "trigger WAL compaction",
		"ja": "WALの整理を実行",
		"ko": "WAL 정리 실행",
		"fr": "déclencher le compactage du WAL",
		"de": "WAL-Verdichtung auslösen",
		"es": "provocar la compactación del WAL",
		"pt": "acionar a compactação do WAL",
		"ru": "запустить сжатие WAL",
		"vi": "kích hoạt dọn dẹp WAL",
	},
	"helpBackup": {
		"zh": "触发备份",
		"en": "trigger a backup",
		"ja": "バックアップを実行",
		"ko": "백업 실행",
		"fr": "déclencher une sauvegarde",
		"de": "Backup auslösen",
		"es": "provocar una copia de seguridad",
		"pt": "acionar uma cópia de segurança",
		"ru": "запустить резервное копирование",
		"vi": "thực hiện sao lưu",
	},
	"helpDura": {
		"zh": "切换持久化模式",
		"en": "switch durability mode",
		"ja": "永続化モードを切替",
		"ko": "영구성 모드 전환",
		"fr": "changer le mode de durabilité",
		"de": "Persistenzmodus wechseln",
		"es": "cambiar el modo de persistencia",
		"pt": "alterar o modo de durabilidade",
		"ru": "переключить режим надёжности",
		"vi": "đổi chế độ bền vững",
	},
	"helpImport": {
		"zh": "批量导入 CSV",
		"en": "bulk import CSV",
		"ja": "CSVを一括インポート",
		"ko": "CSV 일괄 가져오기",
		"fr": "importer en masse un CSV",
		"de": "CSV massenhaft importieren",
		"es": "importar CSV en masa",
		"pt": "importar CSV em massa",
		"ru": "массовый импорт CSV",
		"vi": "nhập CSV hàng loạt",
	},
	"helpExit": {
		"zh": "帮助 / 退出",
		"en": "help / exit",
		"ja": "ヘルプ / 終了",
		"ko": "도움말 / 종료",
		"fr": "aide / quitter",
		"de": "Hilfe / Beenden",
		"es": "ayuda / salir",
		"pt": "ajuda / sair",
		"ru": "справка / выход",
		"vi": "trợ giúp / thoát",
	},
	"rowsInSet": {
		"zh": "%d 行结果",
		"en": "%d rows in set",
		"ja": "%d 件の結果",
		"ko": "%d행 결과",
		"fr": "%d lignes renvoyées",
		"de": "%d Zeilen im Ergebnis",
		"es": "%d filas en el conjunto",
		"pt": "%d linhas no conjunto",
		"ru": "%d строк в наборе",
		"vi": "%d hàng trong tập kết quả",
	},
	"queryOk": {
		"zh": "查询成功，影响 %d 行",
		"en": "Query OK, %d row(s) affected",
		"ja": "クエリ成功、%d 行に影響",
		"ko": "쿼리 성공, %d개 행 변경",
		"fr": "Requête OK, %d ligne(s) affectée(s)",
		"de": "Abfrage erfolgreich, %d Zeile(n) betroffen",
		"es": "Consulta correcta, %d fila(s) afectada(s)",
		"pt": "Consulta OK, %d linha(s) afetada(s)",
		"ru": "Запрос выполнен, затронуто строк: %d",
		"vi": "Truy vấn thành công, %d dòng bị ảnh hưởng",
	},
	"errPrefix": {
		"zh": "错误: %v",
		"en": "ERROR: %v",
		"ja": "エラー: %v",
		"ko": "오류: %v",
		"fr": "ERREUR : %v",
		"de": "FEHLER: %v",
		"es": "ERROR: %v",
		"pt": "ERRO: %v",
		"ru": "ОШИБКА: %v",
		"vi": "LỖI: %v",
	},
	"compactDone": {
		"zh": "WAL 已整理完成",
		"en": "WAL flushed / compacted",
		"ja": "WALを整理・フラッシュしました",
		"ko": "WAL 정리/저장 완료",
		"fr": "WAL vidé / compacté",
		"de": "WAL geleert / verdichtet",
		"es": "WAL vaciado / compactado",
		"pt": "WAL esvaziado / compactado",
		"ru": "WAL сброшен / сжат",
		"vi": "WAL đã được ghi/nén",
	},
	"backupDone": {
		"zh": "备份完成",
		"en": "backup completed",
		"ja": "バックアップ完了",
		"ko": "백업 완료",
		"fr": "sauvegarde terminée",
		"de": "Backup abgeschlossen",
		"es": "copia de seguridad completada",
		"pt": "cópia de segurança concluída",
		"ru": "резервная копия создана",
		"vi": "đã sao lưu xong",
	},
	"durabilitySet": {
		"zh": "持久化模式已设为 %s",
		"en": "durability set to %s",
		"ja": "永続化モードを %s に設定",
		"ko": "영구성 모드가 %s(으)로 설정",
		"fr": "mode de durabilité défini sur %s",
		"de": "Persistenzmodus auf %s gesetzt",
		"es": "modo de persistencia establecido en %s",
		"pt": "modo de durabilidade definido para %s",
		"ru": "режим надёжности установлен: %s",
		"vi": "đã đặt chế độ bền vững thành %s",
	},
	"statRow1": {
		"zh": "命令: %-10s 错误: %-10s qps: %s   tps: %s",
		"en": "commands: %-10s errors: %-10s qps: %s   tps: %s",
		"ja": "コマンド: %-10s エラー: %-10s qps: %s   tps: %s",
		"ko": "명령: %-10s 오류: %-10s qps: %s   tps: %s",
		"fr": "commandes : %-10s erreurs : %-10s qps : %s   tps : %s",
		"de": "Befehle: %-10s Fehler: %-10s qps: %s   tps: %s",
		"es": "comandos: %-10s errores: %-10s qps: %s   tps: %s",
		"pt": "comandos: %-10s erros: %-10s qps: %s   tps: %s",
		"ru": "команды: %-10s ошибок: %-10s qps: %s   tps: %s",
		"vi": "lệnh: %-10s lỗi: %-10s qps: %s   tps: %s",
	},
	"statRow2": {
		"zh": "cpu: %s%%   内存: %s MB   协程: %s   持久化: %s",
		"en": "cpu: %s%%   mem: %s MB   goroutines: %s   durability: %s",
		"ja": "cpu: %s%%   メモリ: %s MB   goroutine: %s   永続化: %s",
		"ko": "cpu: %s%%   메모리: %s MB   고루틴: %s   영구성: %s",
		"fr": "cpu : %s%%   mémoire : %s Mo   goroutines : %s   durabilité : %s",
		"de": "cpu: %s%%   Speicher: %s MB   Goroutinen: %s   Persistenz: %s",
		"es": "cpu: %s%%   memoria: %s MB   goroutines: %s   persistencia: %s",
		"pt": "cpu: %s%%   memória: %s MB   goroutines: %s   durabilidade: %s",
		"ru": "cpu: %s%%   память: %s МБ   горутины: %s   надёжность: %s",
		"vi": "cpu: %s%%   bộ nhớ: %s MB   goroutines: %s   độ bền: %s",
	},
	"statRow3": {
		"zh": "wal: %s MB 已写入",
		"en": "wal: %s MB written",
		"ja": "wal: %s MB 書き込み済み",
		"ko": "wal: %s MB 저장됨",
		"fr": "wal : %s Mo écrits",
		"de": "wal: %s MB geschrieben",
		"es": "wal: %s MB escritos",
		"pt": "wal: %s MB escritos",
		"ru": "wal: %s МБ записано",
		"vi": "wal: %s MB đã ghi",
	},
	"impUsage": {
		"zh": "用法: import --table <表名> --file <data.csv> [--db <数据库>]",
		"en": "usage: import --table <table> --file <data.csv> [--db <database>]",
		"ja": "使い方: import --table <テーブル> --file <data.csv> [--db <DB>]",
		"ko": "사용법: import --table <테이블> --file <data.csv> [--db <데이터베이스>]",
		"fr": "usage : import --table <table> --file <data.csv> [--db <base>]",
		"de": "Verwendung: import --table <Tabelle> --file <data.csv> [--db <Datenbank>]",
		"es": "uso: import --table <tabla> --file <data.csv> [--db <base de datos>]",
		"pt": "uso: import --table <tabela> --file <data.csv> [--db <base de dados>]",
		"ru": "использование: import --table <таблица> --file <data.csv> [--db <база>]",
		"vi": "cách dùng: import --table <bảng> --file <data.csv> [--db <cơ sở dữ liệu>]",
	},
	"impProgress": {
		"zh": "  ...%d 行",
		"en": "  ...%d rows",
		"ja": "  ...%d 件",
		"ko": "  ...%d행",
		"fr": "  ...%d lignes",
		"de": "  ...%d Zeilen",
		"es": "  ...%d filas",
		"pt": "  ...%d linhas",
		"ru": "  ...%d строк",
		"vi": "  ...%d dòng",
	},
	"impDone": {
		"zh": "已导入 %d 行到 %s",
		"en": "imported %d rows into %s",
		"ja": "%d 件を %s にインポート",
		"ko": "%d행을 %s로 가져옴",
		"fr": "%d lignes importées dans %s",
		"de": "%d Zeilen in %s importiert",
		"es": "%d filas importadas en %s",
		"pt": "%d linhas importadas para %s",
		"ru": "импортировано %d строк в %s",
		"vi": "đã nhập %d dòng vào %s",
	},
	"impHeadErr": {
		"zh": "读取表头失败: %v",
		"en": "failed to read header: %v",
		"ja": "ヘッダー読み込み失敗: %v",
		"ko": "헤더 읽기 실패: %v",
		"fr": "échec de lecture de l'en-tête : %v",
		"de": "Kopfzeile lesen fehlgeschlagen: %v",
		"es": "error al leer la cabecera: %v",
		"pt": "falha a ler o cabeçalho: %v",
		"ru": "ошибка чтения заголовка: %v",
		"vi": "lỗi đọc tiêu đề: %v",
	},
	"impLineErr": {
		"zh": "第 %d 行出错: %v",
		"en": "error at line %d: %v",
		"ja": "%d 行目でエラー: %v",
		"ko": "%d행에서 오류: %v",
		"fr": "erreur à la ligne %d : %v",
		"de": "Fehler in Zeile %d: %v",
		"es": "error en la línea %d: %v",
		"pt": "erro na linha %d: %v",
		"ru": "ошибка в строке %d: %v",
		"vi": "lỗi ở dòng %d: %v",
	},
	"impUseErr": {
		"zh": "切换数据库失败 USE %s: %v",
		"en": "USE %s failed: %v",
		"ja": "USE %s に失敗: %v",
		"ko": "USE %s 실패: %v",
		"fr": "échec USE %s : %v",
		"de": "USE %s fehlgeschlagen: %v",
		"es": "USE %s falló: %v",
		"pt": "USE %s falhou: %v",
		"ru": "USE %s не удалось: %v",
		"vi": "USE %s thất bại: %v",
	},
}

var cliLangCode string

func init() {
	cliLangCode = cliDetectLang()
}

// cliDetectLang 按优先级探测语言：TSUMUGI_LANG > LC_ALL > LC_MESSAGES > LANG。
func cliDetectLang() string {
	for _, env := range []string{"TSUMUGI_LANG", "LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(env); v != "" {
			if code := cliNormalizeLang(v); code != "" {
				return code
			}
		}
	}
	return "zh"
}

// cliNormalizeLang 将形如 ja / en_US.UTF-8 / zh-CN 的值归一为支持的语言代码。
func cliNormalizeLang(s string) string {
	short := strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(short, "-_."); i > 0 {
		short = short[:i]
	}
	for _, l := range cliLangList {
		if l == short {
			return l
		}
	}
	if short == "en" {
		return "en"
	}
	return ""
}

// tr 按探测语言取文案；缺键时回退中文，再回退键名。
func tr(key string, args ...interface{}) string {
	s := cliDict[key][cliLangCode]
	if s == "" && cliLangCode != "zh" {
		s = cliDict[key]["zh"]
	}
	if s == "" {
		return key
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

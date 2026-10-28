// resultMessage is the text shown for a backend ActionResult (a toast, the
// editor alert, the test panel). Backend messages stay Japanese by design
// (phase2-contract §7), so in English the text comes from the result code:
// result.code.<code> when the dictionary has it, otherwise a generic English
// text that names the code. A message without Japanese text (an OS or SSH
// error string) is shown as is. Japanese mode always shows the message.

export const RESULT_I18N = {
  ja: {
    'result.done': '完了しました',
    'result.failed': '操作に失敗しました',
    'result.failedCode': '操作に失敗しました（{code}）',
    'result.code.not_found': '対象が見つかりません',
    'result.code.not_pending': 'このリクエストはもう承認待ちではありません',
    'result.code.no_session': '接続中のセッションがありません',
    'result.code.in_use': '使用中のため操作できません',
    'result.code.shutting_down': 'アプリを終了中です',
    'result.code.storage': '保存に失敗しました',
    'result.code.invalid_id': 'ID が不正です',
    'result.code.duplicate_id': '同じ ID がすでにあります',
    'result.code.password_required': 'パスワードを入力してください',
    'result.code.invalid_mcp_path': 'MCP パスが不正です',
    'result.code.expired': '期限が切れています',
    'result.code.required': '必須の項目が入力されていません',
    'result.code.duplicate_name': '同じ名前の接続先がすでにあります',
    'result.code.not_password_auth': 'パスワード認証の接続先ではありません',
    'result.code.keyring_unavailable': 'キーチェーンを使えないため保存できません',
    'result.code.needs_passphrase': '鍵のパスフレーズが必要です',
    'result.code.needs_password': 'パスワードが必要です',
    'result.code.connect_failed': '接続に失敗しました',
    'result.code.canceled': '中止しました',
    'result.code.exec_failed': 'コマンドの実行に失敗しました',
    'result.code.state_storage': 'リクエストの状態を保存できませんでした',
    'result.code.target_unknown': '実行先が記録されていない要求のため承認できません',
    'result.code.restart_failed': 'MCP サーバーの再起動に失敗しました',
    'result.code.invalid_listen': '待ち受け設定が不正です',
    'result.code.token_from_env': '環境変数 SSH_GETE_TOKEN でトークンが指定されているため発行できません',
    'result.code.token_failed': 'トークンを作成できませんでした',
    'result.code.resize_failed': 'サイズ変更に失敗しました',
    'result.code.port_unavailable': 'シリアルポートを確認できません',
    'result.code.port_in_use': 'シリアルポートは使用中です',
    'result.code.input_timeout': '入力の送信が詰まっています',
  },
  en: {
    'result.done': 'Done',
    'result.failed': 'The operation failed',
    'result.failedCode': 'The operation failed ({code})',
    'result.code.not_found': 'Not found',
    'result.code.not_pending': 'This request is no longer pending',
    'result.code.no_session': 'There is no open session',
    'result.code.in_use': 'It is in use and cannot be changed now',
    'result.code.shutting_down': 'The app is shutting down',
    'result.code.storage': 'Saving failed',
    'result.code.invalid_id': 'The ID is not valid',
    'result.code.duplicate_id': 'The ID already exists',
    'result.code.password_required': 'Enter a password',
    'result.code.invalid_mcp_path': 'The MCP path is not valid',
    'result.code.expired': 'It has expired',
    'result.code.required': 'A required field is empty',
    'result.code.duplicate_name': 'A connection with this name already exists',
    'result.code.not_password_auth': 'This connection does not use password authentication',
    'result.code.keyring_unavailable': 'Cannot save because the keychain is unavailable',
    'result.code.needs_passphrase': 'The key needs a passphrase',
    'result.code.needs_password': 'A password is needed',
    'result.code.connect_failed': 'The connection failed',
    'result.code.canceled': 'Canceled',
    'result.code.exec_failed': 'The command failed to run',
    'result.code.state_storage': 'The request state could not be saved',
    'result.code.target_unknown': 'Cannot approve: the request has no recorded target',
    'result.code.restart_failed': 'The MCP server could not be restarted',
    'result.code.invalid_listen': 'The listen settings are not valid',
    'result.code.token_from_env': 'A token cannot be issued here because SSH_GETE_TOKEN is set',
    'result.code.token_failed': 'The token could not be created',
    'result.code.resize_failed': 'Resizing failed',
    'result.code.port_unavailable': 'The serial port cannot be checked',
    'result.code.port_in_use': 'The serial port is in use',
    'result.code.input_timeout': 'Sending input is stuck',
  },
};

const JAPANESE = /[　-ヿ㐀-鿿＀-￯]/;

// dict is the English dictionary (to tell a known code from a missing key);
// t formats a key with variables.
export function resultMessage(result, { lang, t, dict }) {
  const message = String(result?.message ?? '');
  if (lang !== 'en') return message;
  const code = String(result?.code ?? '');
  const key = `result.code.${code}`;
  if (code && dict?.[key]) return t(key);
  if (!message) return '';
  if (!JAPANESE.test(message)) return message;
  if (result?.ok) return t('result.done');
  return code ? t('result.failedCode', { code }) : t('result.failed');
}

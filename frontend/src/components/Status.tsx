// Her ekranın ortak yükleniyor / boş / hata durumları.

export function Loading({ label = 'Yükleniyor…' }: { label?: string }) {
  return (
    <p className="muted" role="status">
      {label}
    </p>
  )
}

export function Empty({ children }: { children: React.ReactNode }) {
  return <p className="empty">{children}</p>
}

export function ErrorBox({ message, onRetry }: { message: string; onRetry?: () => void }) {
  return (
    <div className="error" role="alert">
      <span>{message}</span>
      {onRetry && (
        <button type="button" className="link" onClick={onRetry}>
          Tekrar dene
        </button>
      )}
    </div>
  )
}

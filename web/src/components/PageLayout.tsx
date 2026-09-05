import { createPortal } from 'react-dom'

export function Empty({icon,title,text}:{icon:React.ReactNode;title:string;text?:string}){return <div className="empty-state"><div>{icon}</div><h2>{title}</h2>{text&&<p>{text}</p>}</div>}
export function FloatingPageActions({children}:{children:React.ReactNode}){
	return createPortal(<div className="floating-page-actions">{children}</div>,document.body)
}

import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Eye, EyeOff } from 'lucide-react'

type PasswordInputProps=Omit<React.InputHTMLAttributes<HTMLInputElement>,'type'>

export function PasswordInput(props:PasswordInputProps){
	const {t}=useTranslation()
	const [visible,setVisible]=useState(false)
	const label=t(visible?'common.hidePassword':'common.showPassword')
	return <div className="password-input"><input {...props} type={visible?'text':'password'}/><button type="button" aria-label={label} aria-pressed={visible} title={label} onClick={()=>setVisible(value=>!value)}>{visible?<EyeOff size={16}/>:<Eye size={16}/>}</button></div>
}

import { createContext, useContext } from 'react'

export type NotificationTone='success'|'error'
export type AppNotification={id:string;message:string;tone:NotificationTone}
export type NotificationSink=(message:string,tone?:NotificationTone)=>void
export const NotificationContext=createContext<NotificationSink>(()=>{})
export function useNotifier(){return useContext(NotificationContext)}
